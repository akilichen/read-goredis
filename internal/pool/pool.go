package pool

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/v7/internal"
)

var ErrClosed = errors.New("redis: client is closed")
var ErrPoolTimeout = errors.New("redis: connection pool timeout")

var timers = sync.Pool{
	New: func() interface{} {
		t := time.NewTimer(time.Hour)
		t.Stop()
		return t
	},
}

// Stats contains pool state information and accumulated stats.
type Stats struct {
	Hits     uint32 // number of times free connection was found in the pool
	Misses   uint32 // number of times free connection was NOT found in the pool
	Timeouts uint32 // number of times a wait timeout occurred

	TotalConns uint32 // number of total connections in the pool
	IdleConns  uint32 // number of idle connections in the pool
	StaleConns uint32 // number of stale connections removed from the pool
}

type Pooler interface {
	NewConn(context.Context) (*Conn, error)
	CloseConn(*Conn) error

	Get(context.Context) (*Conn, error)
	Put(*Conn)
	Remove(*Conn, error)

	Len() int
	IdleLen() int
	Stats() *Stats

	Close() error
}

type Options struct {
	Dialer  func(context.Context) (net.Conn, error)
	OnClose func(*Conn) error

	PoolSize           int
	MinIdleConns       int
	MaxConnAge         time.Duration
	PoolTimeout        time.Duration
	IdleTimeout        time.Duration
	IdleCheckFrequency time.Duration
}

type ConnPool struct {
	opt *Options

	dialErrorsNum uint32 // atomic

	lastDialErrorMu sync.RWMutex
	lastDialError   error

	queue chan struct{}

	connsMu      sync.Mutex
	conns        []*Conn
	idleConns    []*Conn
	poolSize     int
	idleConnsLen int

	stats Stats

	_closed  uint32 // atomic
	closedCh chan struct{}
}

var _ Pooler = (*ConnPool)(nil)

func NewConnPool(opt *Options) *ConnPool { // 初始化redis连接池
	p := &ConnPool{
		opt: opt,

		queue:     make(chan struct{}, opt.PoolSize), // 存储正在工作的连接队列
		conns:     make([]*Conn, 0, opt.PoolSize),    //存储连接队列
		idleConns: make([]*Conn, 0, opt.PoolSize),    //存储空闲队列
		closedCh:  make(chan struct{}),               // 用于通知连接池关闭队列，用于通知所有被创建的子协程
	}

	p.checkMinIdleConns() //检查连接池中的空闲连接的数量，若小于则创建足够的空闲连接

	// 若连接池配置中指定了具体的空闲连接超时时间和检查空闲连接频率，启动一个协程用于清理空闲协程
	if opt.IdleTimeout > 0 && opt.IdleCheckFrequency > 0 {
		go p.reaper(opt.IdleCheckFrequency) // 清理协程
	}

	return p
}

func (p *ConnPool) checkMinIdleConns() { // 检查最小空闲连接函数
	if p.opt.MinIdleConns == 0 {
		return
	}
	for p.poolSize < p.opt.PoolSize && p.idleConnsLen < p.opt.MinIdleConns {
		p.poolSize++
		p.idleConnsLen++
		go func() {
			err := p.addIdleConn() // 对上面增加的连接池连接数量进行扩增
			if err != nil {
				p.connsMu.Lock()
				p.poolSize--
				p.idleConnsLen--
				p.connsMu.Unlock()
			}
		}()
	}
}

func (p *ConnPool) addIdleConn() error {
	cn, err := p.dialConn(context.TODO(), true) // ping redis，成功创建并返回一个连接
	if err != nil {
		return err
	}

	p.connsMu.Lock()                      // 连接池队列上锁
	p.conns = append(p.conns, cn)         // 将新创建的连接放入连接池
	p.idleConns = append(p.idleConns, cn) //将新创建的连接放入空闲连接池
	p.connsMu.Unlock()
	return nil
}

// 该函数用于连接池被创建后手动创建的一个连接，pooled参数默认为false，并不默认放入已存在的连接池中。
func (p *ConnPool) NewConn(ctx context.Context) (*Conn, error) {
	return p.newConn(ctx, false)
}

func (p *ConnPool) newConn(ctx context.Context, pooled bool) (*Conn, error) {
	cn, err := p.dialConn(ctx, pooled) // 创建连接
	if err != nil {
		return nil, err
	}

	p.connsMu.Lock()
	p.conns = append(p.conns, cn)
	if pooled {
		// If pool is full remove the cn on next Put.
		// 连接池超过配置的连接池连接数据最大值，pooled不被设置为true，该连接将会在下次放置连接时被移除
		// 解释下不先判断是否满再创建，这样不是省去移除步骤么？是因为有时确实需要一个连接，如连接满了，需要一个
		// 暂时性的连接来被临时创建使用。
		if p.poolSize >= p.opt.PoolSize {
			cn.pooled = false
		} else {
			p.poolSize++ // 否则放入连接池中。
		}
	}
	p.connsMu.Unlock()
	return cn, nil
}

func (p *ConnPool) dialConn(ctx context.Context, pooled bool) (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}

	if atomic.LoadUint32(&p.dialErrorsNum) >= uint32(p.opt.PoolSize) {
		return nil, p.getLastDialError()
	}

	netConn, err := p.opt.Dialer(ctx) //确定设置的redis连接地址能否被ping到
	if err != nil {
		p.setLastDialError(err)
		if atomic.AddUint32(&p.dialErrorsNum, 1) == uint32(p.opt.PoolSize) {
			go p.tryDial()
		}
		return nil, err
	}

	cn := NewConn(netConn) // 创建一个连接并返回
	cn.pooled = pooled
	return cn, nil
}

func (p *ConnPool) tryDial() {
	for { // 当redis连接池未关闭时，不断的ping redis连接。连接池关闭则退出协程，否则直到ping到连接，清空连接池的连接错误次数。
		if p.closed() {
			return
		}

		conn, err := p.opt.Dialer(context.Background())
		if err != nil {
			p.setLastDialError(err)
			time.Sleep(time.Second)
			continue
		}

		atomic.StoreUint32(&p.dialErrorsNum, 0)
		_ = conn.Close()
		return
	}
}

func (p *ConnPool) setLastDialError(err error) {
	p.lastDialErrorMu.Lock()
	p.lastDialError = err
	p.lastDialErrorMu.Unlock()
}

func (p *ConnPool) getLastDialError() error {
	p.lastDialErrorMu.RLock()
	err := p.lastDialError
	p.lastDialErrorMu.RUnlock()
	return err
}

// Get returns existed connection from the pool or creates a new one.
func (p *ConnPool) Get(ctx context.Context) (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}

	err := p.waitTurn(ctx)
	if err != nil {
		return nil, err
	}

	for {
		p.connsMu.Lock()
		cn := p.popIdle() // 取得连接池空闲队列
		p.connsMu.Unlock()

		if cn == nil {
			break
		}

		if p.isStaleConn(cn) { // stale 检查取出的连接是否大于配置的最大存在时间，或者运行的超时时间。
			_ = p.CloseConn(cn)
			continue
		}

		atomic.AddUint32(&p.stats.Hits, 1) // 记录连接复用次数
		return cn, nil
	}

	atomic.AddUint32(&p.stats.Misses, 1) // 记录未从连接池中拿到连接次数

	newcn, err := p.newConn(ctx, true) // 未取得连接，及重新创建连接，并默认放入连接池中。
	if err != nil {
		p.freeTurn() // 释放连接，把连接池工作队列中出队
		return nil, err
	}

	return newcn, nil
}

func (p *ConnPool) getTurn() {
	p.queue <- struct{}{}
}

func (p *ConnPool) waitTurn(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	select {
	case p.queue <- struct{}{}: // ？
		return nil
	default:
	}

	timer := timers.Get().(*time.Timer)
	timer.Reset(p.opt.PoolTimeout)

	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		timers.Put(timer)
		return ctx.Err()
	case p.queue <- struct{}{}:
		if !timer.Stop() {
			<-timer.C
		}
		timers.Put(timer)
		return nil
	case <-timer.C:
		timers.Put(timer)
		atomic.AddUint32(&p.stats.Timeouts, 1)
		return ErrPoolTimeout
	}
}

func (p *ConnPool) freeTurn() {
	<-p.queue // 出连接池工作队列
}

func (p *ConnPool) popIdle() *Conn {
	if len(p.idleConns) == 0 {
		return nil
	}
	// 空闲连接队列减一，并检查是否需要增加空闲连接
	idx := len(p.idleConns) - 1
	cn := p.idleConns[idx]
	p.idleConns = p.idleConns[:idx]
	p.idleConnsLen--
	p.checkMinIdleConns()
	return cn
}

func (p *ConnPool) Put(cn *Conn) { // 用完放回连接池
	if cn.rd.Buffered() > 0 { // 还有写缓冲
		internal.Logger.Printf("Conn has unread data")
		p.Remove(cn, BadConnError{})
		return
	}

	if !cn.pooled { // 若之前临时创建的连接，没有默认放入连接池属性的，会在这里被移除释放
		p.Remove(cn, nil)
		return
	}

	p.connsMu.Lock()
	p.idleConns = append(p.idleConns, cn) // 放入连接池的空闲队列
	p.idleConnsLen++
	p.connsMu.Unlock()
	p.freeTurn() // 释放工作队列，标记为未工作状态
}

func (p *ConnPool) Remove(cn *Conn, reason error) {
	p.removeConnWithLock(cn)
	p.freeTurn()
	_ = p.closeConn(cn)
}

func (p *ConnPool) CloseConn(cn *Conn) error {
	p.removeConnWithLock(cn)
	return p.closeConn(cn)
}

func (p *ConnPool) removeConnWithLock(cn *Conn) {
	p.connsMu.Lock()
	p.removeConn(cn)
	p.connsMu.Unlock()
}

func (p *ConnPool) removeConn(cn *Conn) {
	for i, c := range p.conns {
		if c == cn {
			p.conns = append(p.conns[:i], p.conns[i+1:]...)
			if cn.pooled {
				p.poolSize--
				p.checkMinIdleConns()
			}
			return
		}
	}
}

func (p *ConnPool) closeConn(cn *Conn) error {
	if p.opt.OnClose != nil {
		_ = p.opt.OnClose(cn)
	}
	return cn.Close()
}

// Len returns total number of connections.
func (p *ConnPool) Len() int {
	p.connsMu.Lock()
	n := len(p.conns)
	p.connsMu.Unlock()
	return n
}

// IdleLen returns number of idle connections.
func (p *ConnPool) IdleLen() int {
	p.connsMu.Lock()
	n := p.idleConnsLen
	p.connsMu.Unlock()
	return n
}

func (p *ConnPool) Stats() *Stats {
	idleLen := p.IdleLen()
	return &Stats{
		Hits:     atomic.LoadUint32(&p.stats.Hits),
		Misses:   atomic.LoadUint32(&p.stats.Misses),
		Timeouts: atomic.LoadUint32(&p.stats.Timeouts),

		TotalConns: uint32(p.Len()),
		IdleConns:  uint32(idleLen),
		StaleConns: atomic.LoadUint32(&p.stats.StaleConns),
	}
}

func (p *ConnPool) closed() bool {
	return atomic.LoadUint32(&p._closed) == 1
}

func (p *ConnPool) Filter(fn func(*Conn) bool) error {
	var firstErr error
	p.connsMu.Lock()
	for _, cn := range p.conns {
		if fn(cn) {
			if err := p.closeConn(cn); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	p.connsMu.Unlock()
	return firstErr
}

func (p *ConnPool) Close() error {
	if !atomic.CompareAndSwapUint32(&p._closed, 0, 1) {
		return ErrClosed
	}
	close(p.closedCh)

	var firstErr error
	p.connsMu.Lock()
	for _, cn := range p.conns {
		if err := p.closeConn(cn); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	p.conns = nil
	p.poolSize = 0
	p.idleConns = nil
	p.idleConnsLen = 0
	p.connsMu.Unlock()

	return firstErr
}

func (p *ConnPool) reaper(frequency time.Duration) {
	ticker := time.NewTicker(frequency)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// It is possible that ticker and closedCh arrive together,
			// and select pseudo-randomly pick ticker case, we double
			// check here to prevent being executed after closed.
			if p.closed() {
				return
			}
			_, err := p.ReapStaleConns()
			if err != nil {
				internal.Logger.Printf("ReapStaleConns failed: %s", err)
				continue
			}
		case <-p.closedCh:
			return
		}
	}
}

func (p *ConnPool) ReapStaleConns() (int, error) {
	var n int
	for {
		p.getTurn()

		p.connsMu.Lock()
		cn := p.reapStaleConn()
		p.connsMu.Unlock()
		p.freeTurn()

		if cn != nil {
			_ = p.closeConn(cn)
			n++
		} else {
			break
		}
	}
	atomic.AddUint32(&p.stats.StaleConns, uint32(n))
	return n, nil
}

func (p *ConnPool) reapStaleConn() *Conn {
	if len(p.idleConns) == 0 {
		return nil
	}

	cn := p.idleConns[0]
	if !p.isStaleConn(cn) {
		return nil
	}

	p.idleConns = append(p.idleConns[:0], p.idleConns[1:]...)
	p.idleConnsLen--
	p.removeConn(cn)

	return cn
}

func (p *ConnPool) isStaleConn(cn *Conn) bool {
	if p.opt.IdleTimeout == 0 && p.opt.MaxConnAge == 0 {
		return false
	}

	now := time.Now()
	if p.opt.IdleTimeout > 0 && now.Sub(cn.UsedAt()) >= p.opt.IdleTimeout {
		return true
	}
	if p.opt.MaxConnAge > 0 && now.Sub(cn.createdAt) >= p.opt.MaxConnAge {
		return true
	}

	return false
}
