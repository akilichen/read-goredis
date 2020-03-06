package pool

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/v7/internal/proto"
)

var noDeadline = time.Time{}

type Conn struct {
	netConn net.Conn // 基于TCP的网络连接

	rd *proto.Reader //根据redis协议实现的读
	wr *proto.Writer //根据redis协议实现的写

	Inited    bool      //是否完成初始化
	pooled    bool      // 是否放进连接池
	createdAt time.Time // 连接创建时间
	usedAt    int64     // atomic，使用时间
}

func NewConn(netConn net.Conn) *Conn {
	cn := &Conn{
		netConn:   netConn,
		createdAt: time.Now(),
	}
	cn.rd = proto.NewReader(netConn)
	cn.wr = proto.NewWriter(netConn)
	cn.SetUsedAt(time.Now())
	return cn
}

func (cn *Conn) UsedAt() time.Time {
	unix := atomic.LoadInt64(&cn.usedAt)
	return time.Unix(unix, 0)
}

func (cn *Conn) SetUsedAt(tm time.Time) {
	atomic.StoreInt64(&cn.usedAt, tm.Unix())
}

func (cn *Conn) SetNetConn(netConn net.Conn) {
	cn.netConn = netConn
	cn.rd.Reset(netConn)
	cn.wr.Reset(netConn)
}

func (cn *Conn) Write(b []byte) (int, error) {
	return cn.netConn.Write(b)
}

func (cn *Conn) RemoteAddr() net.Addr {
	return cn.netConn.RemoteAddr()
}

func (cn *Conn) WithReader(ctx context.Context, timeout time.Duration, fn func(rd *proto.Reader) error) error {
	err := cn.netConn.SetReadDeadline(cn.deadline(ctx, timeout))
	if err != nil {
		return err
	}
	return fn(cn.rd)
}

func (cn *Conn) WithWriter(
	ctx context.Context, timeout time.Duration, fn func(wr *proto.Writer) error,
) error {
	err := cn.netConn.SetWriteDeadline(cn.deadline(ctx, timeout))
	if err != nil {
		return err
	}

	if cn.wr.Buffered() > 0 {
		cn.wr.Reset(cn.netConn)
	}

	err = fn(cn.wr)
	if err != nil {
		return err
	}

	return cn.wr.Flush()
}

func (cn *Conn) Close() error {
	return cn.netConn.Close()
}

func (cn *Conn) deadline(ctx context.Context, timeout time.Duration) time.Time {
	tm := time.Now()
	cn.SetUsedAt(tm)

	if timeout > 0 {
		tm = tm.Add(timeout)
	}

	if ctx != nil {
		deadline, ok := ctx.Deadline()
		if ok {
			if timeout == 0 {
				return deadline
			}
			if deadline.Before(tm) {
				return deadline
			}
			return tm
		}
	}

	if timeout > 0 {
		return tm
	}

	return noDeadline
}
