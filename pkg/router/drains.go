package router

import (
	"errors"
	"github.com/akkeris/logtrain/internal/debug"
	"github.com/akkeris/logtrain/pkg/output"
	"github.com/trevorlinton/remote_syslog2/syslog"
	"hash/crc32"
	"sync"
)

/*
 * Responsibilities:
 * - Pooling connections and distributing incoming messages over pools
 * - Detecting back pressure and increasing pools
 * - Decreasing pools if output disconnects or if pressure is normal
 * - Keeping buffer of information coming off of input
 * - Reporting information (metrics) or errors to upstream
 *
 * Principals:
 * - Only create one drain per endpoint.  If it can be pooled, it will
 * - Propogate up all errors, assume we're still good to go unless explicitly closed.
 */

const increasePercentTrigger = 0.5  // > 50% full.
const decreasePercentTrigger = 0.02 // 2% full.
const bufferSize = 512              // amount of records to keep in memory until upstream fails.

type Drain struct {
	Input          chan syslog.Packet
	Info           chan string
	Error          chan error
	Endpoint       string
	maxconnections uint32
	errors         uint32
	connections    []output.Output
	mutex          *sync.Mutex
	sent           uint32
	stop           chan struct{}
	pressure       float64
	open           uint32
	sticky         bool
	transportPools bool
}

func Create(endpoint string, maxconnections uint32, sticky bool) (*Drain, error) {
	if maxconnections > 1024 {
		return nil, errors.New("Max connections must not be more than 1024.")
	}
	if maxconnections == 0 {
		return nil, errors.New("Max connections must not be 0.")
	}
	drain := Drain{
		Endpoint:       endpoint,
		maxconnections: maxconnections,
		errors:         0,
		sent:           0,
		pressure:       0,
		open:           0,
		sticky:         sticky,
		transportPools: false,
		Input:          make(chan syslog.Packet, bufferSize),
		Info:           make(chan string, 1),
		Error:          make(chan error, 1),
		connections:    make([]output.Output, 0),
		mutex:          &sync.Mutex{},
		stop:           make(chan struct{}, 1),
	}

	if err := output.TestEndpoint(endpoint); err != nil {
		return nil, err
	}
	return &drain, nil
}

func (drain *Drain) MaxConnections() uint32 {
	return drain.maxconnections
}

func (drain *Drain) OpenConnections() uint32 {
	return drain.open
}

func (drain *Drain) Pressure() float64 {
	return drain.pressure
}

func (drain *Drain) Sent() uint32 {
	return drain.sent
}

func (drain *Drain) Errors() uint32 {
	return drain.errors
}

func (drain *Drain) ResetMetrics() {
	drain.sent = 0
	drain.errors = 0
}

func (drain *Drain) Dial() error {
	debug.Debugf("[drains] Dailing drain %s...\n", drain.Endpoint)
	if drain.open != 0 {
		return errors.New("Dial should not be called twice.")
	}
	if err := drain.connect(); err != nil {
		debug.Debugf("[drains] a call to connect resulted in an error: %s\n", err.Error())
		return err
	}

	if drain.transportPools == true {
		go drain.loopTransportPools()
	} else if drain.sticky == true {
		go drain.loopSticky()
	} else {
		go drain.loopRoundRobin()
	}
	return nil
}

func (drain *Drain) Close() error {
	debug.Debugf("[drains] Closing drain to %s\n", drain.Endpoint)
	drain.stop <- struct{}{}
	drain.mutex.Lock()
	defer drain.mutex.Unlock()
	var err error = nil
	for _, conn := range drain.connections {
		debug.Debugf("[drains] Closing connection to %s\n", drain.Endpoint)
		if err = conn.Close(); err != nil {
			debug.Debugf("[drains] Received error trying to close connection to %s: %s\n", drain.Endpoint, err.Error())
		}
		drain.open--
	}
	drain.connections = make([]output.Output, 0)
	return err
}

func (drain *Drain) connect() error {
	debug.Debugf("[drains] Drain %s connecting...\n", drain.Endpoint)
	drain.mutex.Lock()
	defer drain.mutex.Unlock()
	conn, err := output.Create(drain.Endpoint)
	if err != nil {
		debug.Errorf("[drains] Received an error attempting to create endpoint %s: %s\n", drain.Endpoint, err.Error())
		return err
	}
	if err := conn.Dial(); err != nil {
		debug.Errorf("[drains] Received an error attempting to dial %s: %s\n", drain.Endpoint, err.Error())
		return err
	}

	drain.transportPools = conn.Pools()
	drain.connections = append(drain.connections, conn)
	drain.open++
	debug.Debugf("[drains] Opening new connection to %s\n", drain.Endpoint)

	go func() {
		for {
			select {
			case err := <-conn.Errors():
				if err != nil {
					debug.Errorf("[drains] Received an error from output on %s: %s\n", drain.Endpoint, err.Error())
					drain.errors++
				} else {
					debug.Debugf("[drains] Received a nil on error channel (assuming it closed). %s\n", drain.Endpoint)
					// The error channel was closed, stop watching.
					return
				}
			case <-drain.stop:
				debug.Debugf("[drains] Received stop message for drain %s\n", drain.Endpoint)
				return
			}
		}
	}()
	return nil
}

/*
 * The write loop functions below are critical paths, removing as much operations in these as possible
 * is important to performance. An if statement to use a sticky or round robin
 * strategy in the drain is therefore pushed up to the Dail function, unfortuntely
 * this does mean there's some repetitive code.
 */

func (drain *Drain) loopRoundRobin() {
	var maxPackets = cap(drain.Input)
	for {
		select {
		case packet := <-drain.Input:
			drain.mutex.Lock()
			drain.sent++
			drain.connections[drain.sent%drain.open].Packets() <- packet
			drain.pressure = (drain.pressure + (float64(len(drain.Input)) / float64(maxPackets))) / float64(2)
			if drain.pressure > increasePercentTrigger && drain.open < drain.maxconnections {
				debug.Debugf("[drains] Increasing pool size %s to %d because back pressure was %f%%\n", drain.Endpoint, drain.open, drain.pressure*100)
				go drain.connect()
			}
			drain.mutex.Unlock()
		case <-drain.stop:
			return
		}
	}
}

func (drain *Drain) loopSticky() {
	var maxPackets = cap(drain.Input)
	for {
		select {
		case packet := <-drain.Input:
			drain.mutex.Lock()
			drain.sent++
			drain.connections[uint32(crc32.ChecksumIEEE([]byte(packet.Hostname+packet.Tag))%drain.open)].Packets() <- packet
			drain.pressure = (drain.pressure + (float64(len(drain.Input)) / float64(maxPackets))) / float64(2)
			if drain.pressure > increasePercentTrigger && drain.open < drain.maxconnections {
				debug.Debugf("[drains] Increasing pool size %s to %d because back pressure was %f%%\n", drain.Endpoint, drain.open, drain.pressure*100)
				go drain.connect()
			}
			drain.mutex.Unlock()
		case <-drain.stop:
			return
		}
	}
}

func (drain *Drain) loopTransportPools() {
	var maxPackets = cap(drain.Input)
	for {
		select {
		case packet := <-drain.Input:
			drain.mutex.Lock()
			drain.sent++
			drain.connections[0].Packets() <- packet
			drain.pressure = (drain.pressure + (float64(len(drain.Input)) / float64(maxPackets))) / float64(2)
			drain.mutex.Unlock()
		case <-drain.stop:
			return
		}
	}
}
