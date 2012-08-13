package client

import (
	"github.com/4ad/doozer"
	"github.com/bketelsen/skynet"
	"github.com/bketelsen/skynet/pools"
	"github.com/bketelsen/skynet/service"
	"launchpad.net/mgo/v2/bson"
	"time"
)

type serviceError struct {
	error
}

type ServiceClient struct {
	client  *Client
	Log     skynet.Logger `json:"-"`
	cconfig *skynet.ClientConfig
	query   *Query
	// a list of the known instances
	instances map[string]*servicePool
	// a pool of the available instances. contains things of type servicePool
	instancePool *pools.ResourcePool
	muxChan      chan interface{}
	timeoutChan  chan timeoutLengths

	instanceListener *InstanceListener
	listenID         string

	retryTimeout  time.Duration
	giveupTimeout time.Duration
}

func newServiceClient(query *Query, c *Client) (sc *ServiceClient) {
	sc = &ServiceClient{
		client:       c,
		Log:          c.Config.Log,
		cconfig:      c.Config,
		query:        query,
		instances:    make(map[string]*servicePool),
		instancePool: pools.NewResourcePool(func() (pools.Resource, error) { panic("unreachable") }, -1, 0),
		muxChan:      make(chan interface{}),
		timeoutChan:  make(chan timeoutLengths),
	}
	sc.listenID = skynet.UUID()
	sc.instanceListener = c.instanceMonitor.Listen(sc.listenID, query)

	go sc.mux()
	return
}

type instanceFileCollector struct {
	files []string
}

func (ic *instanceFileCollector) VisitDir(path string, f *doozer.FileInfo) bool {
	return true
}
func (ic *instanceFileCollector) VisitFile(path string, f *doozer.FileInfo) {
	ic.files = append(ic.files, path)
}

type servicePool struct {
	service *service.Service
	pool    *pools.ResourcePool
	closed  bool
}

// this is here to make it a pools.Resource
func (sp *servicePool) Close() {
	sp.closed = true
}

// this is here to make it a pools.Resource
func (sp *servicePool) IsClosed() bool {
	return sp.closed
}

type timeoutLengths struct {
	retry, giveup time.Duration
}

func (c *ServiceClient) addInstanceMux(instance *service.Service) {
	m := service.ServiceDiscovered{instance}
	key := m.Service.Config.ServiceAddr.String()
	_, known := c.instances[key]
	if !known {
		// we got a new pool, put it into the wild
		c.instances[key] = c.client.getServicePool(m.Service)
		c.instancePool.Release(c.instances[key])
		c.Log.Item(m)
	}
}

func (c *ServiceClient) removeInstanceMux(instance *service.Service) {
	m := service.ServiceRemoved{instance}
	key := m.Service.Config.ServiceAddr.String()
	_, known := c.instances[key]
	if !known {
		return
	}
	c.instances[key].Close()
	delete(c.instances, m.Service.Config.ServiceAddr.String())
	c.Log.Item(m)
}

func (c *ServiceClient) mux() {

	for {
		select {
		case ns := <-c.instanceListener.NotificationChan:
			for _, n := range ns {
				switch n.Type {
				case InstanceAddNotification, InstanceUpdateNotification:
					if n.Service.Registered {
						c.addInstanceMux(&n.Service)
					} else {
						c.removeInstanceMux(&n.Service)
					}
				case InstanceRemoveNotification:
					c.removeInstanceMux(&n.Service)
				}
			}
		case c.timeoutChan <- timeoutLengths{
			retry:  c.retryTimeout,
			giveup: c.giveupTimeout,
		}:

		}
	}
}

/*
ServiceClient.SetTimeout() sets the time before ServiceClient.Send() retries requests, and
the time before ServiceClient.Send() and ServiceClient.SendOnce() give up. Setting retry
or giveup to 0 indicates no retry or time out.
*/
func (c *ServiceClient) SetTimeout(retry, giveup time.Duration) {
	c.muxChan <- timeoutLengths{
		retry:  retry,
		giveup: giveup,
	}
}

func (c *ServiceClient) GetTimeout() (retry, giveup time.Duration) {
	tls := <-c.timeoutChan
	retry, giveup = tls.retry, tls.giveup
	return
}

// ServiceClient.sendToInstance() tries to make an RPC request on a particular connection to an instance
func (c *ServiceClient) sendToInstance(sr ServiceResource, requestInfo *skynet.RequestInfo, funcName string, in interface{}) (result []byte, err error) {
	if requestInfo == nil {
		requestInfo = &skynet.RequestInfo{
			RequestID: skynet.UUID(),
		}
	}

	sin := service.ServiceRPCIn{
		RequestInfo: requestInfo,
		Method:      funcName,
	}

	sin.In, err = bson.Marshal(in)
	if err != nil {
		return
	}

	sout := service.ServiceRPCOut{}

	err = sr.rpcClient.Call(sr.service.Config.Name+".Forward", sin, &sout)
	if err != nil {
		sr.Close()
		c.Log.Item(err)
	}

	if sout.Err != nil {
		err = serviceError{sout.Err}
	}

	result = sout.Out

	return
}

type sendAttempt struct {
	result []byte
	err    error
}

func (c *ServiceClient) attemptSend(attempts chan sendAttempt, ri *skynet.RequestInfo, fn string, in interface{}, out interface{}) {
	// first find an available instance
	spr, _ := c.instancePool.Acquire()
	sp := spr.(*servicePool)
	defer c.instancePool.Release(sp)

	// then, get a connection to that instance
	r, err := sp.pool.Acquire()
	defer sp.pool.Release(r)
	if err != nil {
		c.Log.Item(err)
		attempts <- sendAttempt{err: err}
		return
	}

	sr := r.(ServiceResource)

	result, err := c.sendToInstance(sr, ri, fn, in)

	attempts <- sendAttempt{
		result: result,
		err:    err,
	}
}

/*
ServiceClient.SendOnce() will send a request to one of the available instances. In intervals of retry time,
it will send additional requests to other known instances. If no response is heard after
the giveup time has passed, it will return an error.
*/
func (c *ServiceClient) Send(ri *skynet.RequestInfo, fn string, in interface{}, out interface{}) (err error) {
	retry, giveup := c.GetTimeout()
	return c.send(retry, giveup, ri, fn, in, out)
}

/*
ServiceClient.SendOnce() will send a request to one of the available instances. If no response is heard after
the giveup time has passed, it will return an error.
*/
func (c *ServiceClient) SendOnce(ri *skynet.RequestInfo, fn string, in interface{}, out interface{}) (err error) {
	_, giveup := c.GetTimeout()
	return c.send(0, giveup, ri, fn, in, out)
}

func (c *ServiceClient) send(retry, giveup time.Duration, ri *skynet.RequestInfo, fn string, in interface{}, out interface{}) (err error) {
	attempts := make(chan sendAttempt)

	var ticker <-chan time.Time
	if retry > 0 {
		ticker = time.NewTicker(retry).C
	}

	var timeout <-chan time.Time
	if giveup > 0 {
		timeout = time.NewTimer(giveup).C
	}

	go c.attemptSend(attempts, ri, fn, in, out)

	for {
		select {
		case <-ticker:
			go c.attemptSend(attempts, ri, fn, in, out)
		case <-timeout:
			if err == nil {
				err = ErrRequestTimeout
			}
			// otherwise use the last error reported from an attempt
			return
		case attempt := <-attempts:
			err = attempt.err
			if err != nil {
				if _, ok := err.(serviceError); !ok {
					// error during transmition, abort this attempt
					if giveup == 0 {
						return
					}
					continue
				}
			}

			err = bson.Unmarshal(attempt.result, out)
			return
		}
	}

	return
}