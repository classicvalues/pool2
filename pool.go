// A generic resource pool for databases etc
package pool

import (
	"errors"
	"io"
	"time"
)

var (
	TimeoutError    = errors.New("Timeout")
	PoolClosedError = errors.New("Pool is closed")
)

// ResourceOpener opens a resource
type ResourceOpener interface {
	Open() (Resource, error)
}

type Resource interface {
	io.Closer
	// Good returns true when the resource is in good state
	Good() bool
}

type PooledResource interface {
	// Release releases the resource back to the pool for reuse
	Release() error
	// Destroy destroys the resource. It is no longer usable.
	Destroy() error
	// Resource returns the underlying Resource
	Resource() Resource
}

type PoolMetrics interface {
	ReportResources(stats ResourcePoolStat)
	ReportWait(wt time.Duration)
}

type ResourcePoolStat struct {
	AvailableNow  uint32
	ResourcesOpen uint32
	Cap           uint32
	InUse         uint32
}

// ResourcePool manages a pool of resources for reuse.
type ResourcePool struct {
	metrics PoolMetrics //metrics interface to track how the pool performs

	// To get a resource, get a ticket first and then get the resource from
	// either the reserve or open a new one.
	// To release a resource, first put the resource back to the reserve
	// and then put the ticket back.
	// The order is important to make sure we never create >cap(tickets)
	// number of resources.
	reserve chan Resource // idle resources, ready for use
	tickets chan struct{} // ticket to own resources

	opener ResourceOpener
	closed chan struct{}
}

// New creates a new pool of Clients.
func New(maxReserve, maxOpen uint32, opener ResourceOpener, m PoolMetrics) *ResourcePool {
	if maxOpen < maxReserve {
		panic("maxOpen must be > maxReserve")
	}
	tickets := make(chan struct{}, maxOpen)
	for i := uint32(0); i < maxOpen; i++ {
		tickets <- struct{}{}
	}
	return &ResourcePool{
		metrics: m,
		reserve: make(chan Resource, maxReserve),
		tickets: tickets,
		opener:  opener,
		closed:  make(chan struct{}),
	}
}

func (p *ResourcePool) Get() (PooledResource, error) {
	return p.GetWithTimeout(365 * 24 * time.Hour) // 1 year is forever
}

type pooledResource struct {
	p   *ResourcePool
	res Resource
}

func (pr *pooledResource) Release() error {
	return pr.p.release(pr)
}

func (pr *pooledResource) Destroy() error {
	return pr.p.destroy(pr)
}

func (pr *pooledResource) Resource() Resource {
	return pr.res
}

func (p *ResourcePool) GetWithTimeout(timeout time.Duration) (PooledResource, error) {
	// order is important: first ticket then reserve
	start := time.Now()
	select {
	case <-p.tickets:
	case <-time.After(timeout):
		return nil, TimeoutError
	case <-p.closed:
		return nil, PoolClosedError
	}
	if p.isClosed() {
		return nil, PoolClosedError
	}
	p.reportMetrics(time.Now().Sub(start))

	for {
		select {
		case r := <-p.reserve:
			if r.Good() {
				return &pooledResource{p: p, res: r}, nil
			}
			r.Close()

		default:
			// no reserve
			break
		}
	}

	r, err := p.opener.Open()
	if err != nil {
		// release ticket on error
		p.tickets <- struct{}{}
		return nil, err
	}
	return &pooledResource{p: p, res: r}, nil
}

func (p *ResourcePool) release(pr PooledResource) error {
	var err error
	// order is important: first reserve then ticket
	res := pr.Resource()
	select {
	case p.reserve <- res:
	default:
		// reserve is full
		err = res.Close()
	}
	p.tickets <- struct{}{}

	if p.isClosed() {
		p.drainReserve()
	}
	return err
}

func (p *ResourcePool) destroy(pr PooledResource) error {
	p.tickets <- struct{}{}
	return pr.Resource().Close()
}

// Close closes the pool. Resources in use are not affected.
func (p *ResourcePool) Close() error {
	close(p.closed)
	p.drainReserve()
	return nil
}

func (p *ResourcePool) isClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

func (p *ResourcePool) drainReserve() {
	for {
		select {
		case r := <-p.reserve:
			r.Close()
		default:
		}
	}
}

/**
Metrics
**/
func (p *ResourcePool) reportMetrics(wt time.Duration) {
	if p.metrics != nil {
		go p.metrics.ReportWait(wt)
		go p.metrics.ReportResources(p.Stats())
	}
}

func (p *ResourcePool) Stats() ResourcePoolStat {
	tot := uint32(cap(p.tickets))
	n := uint32(len(p.tickets))
	inuse := tot - n
	available := uint32(len(p.reserve))

	return ResourcePoolStat{
		AvailableNow:  available,
		ResourcesOpen: inuse + available,
		Cap:           tot,
		InUse:         inuse,
	}
}
