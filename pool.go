package gore

import (
	"container/list"
	"sync"
	"time"
)

// Pool is a pool of connection. The application acquires connection
// from pool using Acquire() method, and when done, returns it to the pool
// with Release().
type Pool struct {
	// Request timeout for each connection
	RequestTimeout time.Duration
	// Initial number of connection to open
	InitialConn int
	// Maximum number of connection to open
	MaximumConn int

	l                    *list.List
	currentNumberOfConn  int
	unusableNumberOfConn int
	mutex                *sync.Mutex
	cond                 *sync.Cond
	address              string
	closed               bool
}

// Dial initializes connection from the pool to redis server.
// If the redis server cannot be connected, this function returns
// an error, and the application should fail accordingly.
func (p *Pool) Dial(address string) error {
	if p.RequestTimeout <= 0 {
		p.RequestTimeout = 10 * time.Second
	}
	if p.InitialConn <= 0 {
		p.InitialConn = 5
	}
	if p.MaximumConn <= 0 {
		p.MaximumConn = 10
	}
	if p.MaximumConn < p.InitialConn {
		p.MaximumConn = p.InitialConn
	}
	p.l = list.New()
	p.mutex = &sync.Mutex{}
	p.cond = sync.NewCond(p.mutex)
	p.address = address
	return p.connect(0)
}

// Close properly closes the pool
func (p *Pool) Close() {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	for e := p.l.Front(); e != nil; e = e.Next() {
		conn, _ := e.Value.(*Conn)
		conn.Close()
	}
	p.l.Init()
	p.currentNumberOfConn = p.l.Len()
	p.cond.Broadcast()
}

// Acquire returns a usable, exclusive connection for the goroutine.
// If this function return a nil connection, application can check the
// error to know whether there is really an error or it is because the pool was closed.
// If the pool was closed, the returned error will also be nil.
func (p *Pool) Acquire() (*Conn, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	for p.l.Len() == 0 && !p.closed {
		if p.currentNumberOfConn < p.MaximumConn {
			conn, err := DialTimeout(p.address, 5*time.Second)
			if err != nil {
				return nil, err
			}
			p.l.PushBack(conn)
			p.currentNumberOfConn++
			break
		} else if p.currentNumberOfConn == p.unusableNumberOfConn {
			// All available connections are disconnected. We fail fast here.
			return nil, ErrNotConnected
		} else {
			// Wait
			p.cond.Wait()
			if p.closed {
				// The wait may be broken by a broadcast from close.
				return nil, nil
			}
		}
	}
	if p.closed {
		return nil, nil
	}
	conn, _ := p.l.Remove(p.l.Front()).(*Conn)
	return conn, nil
}

// Release pushs the connection back to the pool. The pool makes sure
// this connection must be usable before pushing it back to the acquirable
// list.
func (p *Pool) Release(conn *Conn) {
	if conn == nil {
		return
	}
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if p.closed {
		return
	}
	p.unusableNumberOfConn++
	go p.pushBack(conn)
}

func (p *Pool) connect(timeout time.Duration) (err error) {
	p.mutex.Lock()
	defer func() {
		if err != nil {
			for e := p.l.Front(); e != nil; e = e.Next() {
				conn, _ := e.Value.(*Conn)
				conn.Close()
			}
			p.l.Init()
		}
		p.currentNumberOfConn = p.l.Len()
		p.mutex.Unlock()
	}()
	if p.l.Len() > 0 {
		return nil
	}
	for i := 0; i < p.InitialConn; i++ {
		conn, err := DialTimeout(p.address, timeout)
		if err != nil {
			return err
		}
		p.l.PushBack(conn)
	}
	return nil
}

func (p *Pool) pushBack(conn *Conn) {
	for {
		if conn.state == connStateConnected {
			p.mutex.Lock()
			p.l.PushBack(conn)
			p.unusableNumberOfConn--
			p.cond.Signal()
			p.mutex.Unlock()
			break
		}
		time.Sleep(2 * time.Second)
	}
}
