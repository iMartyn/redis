package pool

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/internal"
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
	FreeConns  uint32 // deprecated - use IdleConns
	IdleConns  uint32 // number of idle connections in the pool
	StaleConns uint32 // number of stale connections removed from the pool
}

type Pooler interface {
	NewConn() (*Conn, error)
	CloseConn(*Conn) error

	Get() (*Conn, error)
	Put(*Conn)
	Remove(*Conn)

	Len() int
	IdleLen() int
	Stats() *Stats

	Close() error
}

type Options struct {
	Dialer  func() (net.Conn, error)
	OnClose func(*Conn) error

	PoolSize           int
	MinIdleConns       int
	PoolTimeout        time.Duration
	IdleTimeout        time.Duration
	IdleCheckFrequency time.Duration
}

type ConnPool struct {
	opt *Options

	dialErrorsNum uint32 // atomic

	lastDialError   error
	lastDialErrorMu sync.RWMutex

	queue chan struct{}

	connsMu  sync.Mutex
	conns    []*Conn
	poolSize int32 // atomic

	idleConnsMu sync.RWMutex
	idleConns   []*Conn

	stats Stats

	_closed uint32 // atomic
}

var _ Pooler = (*ConnPool)(nil)

func NewConnPool(opt *Options) *ConnPool {
	p := &ConnPool{
		opt: opt,

		queue:     make(chan struct{}, opt.PoolSize),
		conns:     make([]*Conn, 0, opt.PoolSize),
		idleConns: make([]*Conn, 0, opt.PoolSize),
	}

	for i := 0; i < opt.MinIdleConns; i++ {
		p.addIdleConn()
	}

	if opt.IdleTimeout > 0 && opt.IdleCheckFrequency > 0 {
		go p.reaper(opt.IdleCheckFrequency)
	}

	return p
}

func (p *ConnPool) addIdleConn() {
	cn, err := p.newConn(true)
	if err != nil {
		return
	}

	err = p.waitTurn()
	if err != nil {
		p.closeConn(cn)
		return
	}

	p.connsMu.Lock()
	p.idleConnsMu.Lock()

	hasSpace := int(atomic.LoadInt32(&p.poolSize)) < p.opt.PoolSize &&
		len(p.idleConns) < p.opt.MinIdleConns
	if hasSpace {
		p.conns = append(p.conns, cn)
		p.idleConns = append(p.idleConns, cn)
		if cn.pooled {
			atomic.AddInt32(&p.poolSize, 1)
		}
	}

	p.idleConnsMu.Unlock()
	p.connsMu.Unlock()

	if !hasSpace {
		p.closeConn(cn)
	}

	p.freeTurn()
}

func (p *ConnPool) NewConn() (*Conn, error) {
	return p._NewConn(false)
}

func (p *ConnPool) _NewConn(pooled bool) (*Conn, error) {
	cn, err := p.newConn(pooled)
	if err != nil {
		return nil, err
	}

	p.connsMu.Lock()
	p.conns = append(p.conns, cn)
	if cn.pooled {
		atomic.AddInt32(&p.poolSize, 1)
	}
	p.connsMu.Unlock()
	return cn, nil
}

func (p *ConnPool) newConn(pooled bool) (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}

	if atomic.LoadUint32(&p.dialErrorsNum) >= uint32(p.opt.PoolSize) {
		return nil, p.getLastDialError()
	}

	netConn, err := p.opt.Dialer()
	if err != nil {
		p.setLastDialError(err)
		if atomic.AddUint32(&p.dialErrorsNum, 1) == uint32(p.opt.PoolSize) {
			go p.tryDial()
		}
		return nil, err
	}

	cn := NewConn(netConn)
	cn.pooled = pooled
	return cn, nil
}

func (p *ConnPool) tryDial() {
	for {
		if p.closed() {
			return
		}

		conn, err := p.opt.Dialer()
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
func (p *ConnPool) Get() (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}

	err := p.waitTurn()
	if err != nil {
		return nil, err
	}

	for {
		p.idleConnsMu.Lock()
		cn := p.popIdle()
		p.idleConnsMu.Unlock()

		if cn == nil {
			break
		}

		if cn.IsStale(p.opt.IdleTimeout) {
			p.CloseConn(cn)
			continue
		}

		atomic.AddUint32(&p.stats.Hits, 1)
		return cn, nil
	}

	atomic.AddUint32(&p.stats.Misses, 1)

	newcn, err := p._NewConn(true)
	if err != nil {
		p.freeTurn()
		return nil, err
	}

	return newcn, nil
}

func (p *ConnPool) getTurn() {
	p.queue <- struct{}{}
}

func (p *ConnPool) waitTurn() error {
	select {
	case p.queue <- struct{}{}:
		return nil
	default:
		timer := timers.Get().(*time.Timer)
		timer.Reset(p.opt.PoolTimeout)

		select {
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
}

func (p *ConnPool) freeTurn() {
	<-p.queue
}

func (p *ConnPool) popIdle() *Conn {
	if len(p.idleConns) == 0 {
		return nil
	}

	idx := len(p.idleConns) - 1
	cn := p.idleConns[idx]
	p.idleConns = p.idleConns[:idx]
	p.checkMinIdleConns()
	return cn
}

func (p *ConnPool) Put(cn *Conn) {
	buf := cn.Rd.Bytes()
	if len(buf) > 0 {
		internal.Logf("connection has unread data: %.100q", buf)
		p.Remove(cn)
		return
	}

	for p.opt.MinIdleConns > 0 {
		poolSize := atomic.LoadInt32(&p.poolSize)
		if int(poolSize) <= p.opt.PoolSize {
			break
		}
		if atomic.CompareAndSwapInt32(&p.poolSize, poolSize, poolSize-1) {
			cn.pooled = false
			p.Remove(cn)
			return
		}
	}

	p.idleConnsMu.Lock()
	p.idleConns = append(p.idleConns, cn)
	p.idleConnsMu.Unlock()
	p.freeTurn()
}

func (p *ConnPool) Remove(cn *Conn) {
	p.removeConn(cn)
	p.freeTurn()
	_ = p.closeConn(cn)
	p.idleConnsMu.RLock()
	p.checkMinIdleConns()
	p.idleConnsMu.RUnlock()
}

func (p *ConnPool) CloseConn(cn *Conn) error {
	p.removeConn(cn)
	return p.closeConn(cn)
}

func (p *ConnPool) removeConn(cn *Conn) {
	p.connsMu.Lock()
	for i, c := range p.conns {
		if c == cn {
			p.conns = append(p.conns[:i], p.conns[i+1:]...)
			if cn.pooled {
				atomic.AddInt32(&p.poolSize, -1)
			}
			break
		}
	}
	p.connsMu.Unlock()
}

func (p *ConnPool) closeConn(cn *Conn) error {
	if p.opt.OnClose != nil {
		_ = p.opt.OnClose(cn)
	}
	return cn.Close()
}

// Len returns total number of connections.
func (p *ConnPool) Len() int {
	return int(atomic.LoadInt32(&p.poolSize))
}

// FreeLen returns number of idle connections.
func (p *ConnPool) IdleLen() int {
	p.idleConnsMu.RLock()
	l := len(p.idleConns)
	p.idleConnsMu.RUnlock()
	return l
}

func (p *ConnPool) Stats() *Stats {
	idleLen := p.IdleLen()
	return &Stats{
		Hits:     atomic.LoadUint32(&p.stats.Hits),
		Misses:   atomic.LoadUint32(&p.stats.Misses),
		Timeouts: atomic.LoadUint32(&p.stats.Timeouts),

		TotalConns: uint32(p.Len()),
		FreeConns:  uint32(idleLen),
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

	var firstErr error
	p.connsMu.Lock()
	for _, cn := range p.conns {
		if err := p.closeConn(cn); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	p.conns = nil
	atomic.StoreInt32(&p.poolSize, 0)
	p.connsMu.Unlock()

	p.idleConnsMu.Lock()
	p.idleConns = nil
	p.idleConnsMu.Unlock()

	return firstErr
}

func (p *ConnPool) reapStaleConn() *Conn {
	if len(p.idleConns) == 0 {
		return nil
	}

	cn := p.idleConns[0]
	if !cn.IsStale(p.opt.IdleTimeout) {
		return nil
	}

	p.idleConns = append(p.idleConns[:0], p.idleConns[1:]...)
	p.checkMinIdleConns()

	return cn
}

func (p *ConnPool) ReapStaleConns() (int, error) {
	var n int
	for {
		p.getTurn()

		p.idleConnsMu.Lock()
		cn := p.reapStaleConn()
		p.idleConnsMu.Unlock()

		if cn != nil {
			p.removeConn(cn)
		}

		p.freeTurn()

		if cn != nil {
			p.closeConn(cn)
			n++
		} else {
			break
		}
	}
	return n, nil
}

func (p *ConnPool) reaper(frequency time.Duration) {
	ticker := time.NewTicker(frequency)
	defer ticker.Stop()

	for range ticker.C {
		if p.closed() {
			break
		}
		n, err := p.ReapStaleConns()
		if err != nil {
			internal.Logf("ReapStaleConns failed: %s", err)
			continue
		}
		atomic.AddUint32(&p.stats.StaleConns, uint32(n))
	}
}

func (p *ConnPool) checkMinIdleConns() {
	if len(p.idleConns) < p.opt.MinIdleConns {
		go p.addIdleConn()
	}
}
