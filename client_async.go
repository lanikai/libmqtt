/*
 * Copyright Go-IIoT (https://github.com/goiiot)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package libmqtt

import (
	"context"
	"crypto/tls"
	"errors"
	"math"
	"net"
	"sync"
	"time"
)

var (
	// ErrTimeOut connection timeout error
	ErrTimeOut = errors.New("connection timeout ")
)

// Client type for *AsyncClient
type Client = *AsyncClient

// NewClient create a new mqtt client
func NewClient(options ...Option) (Client, error) {
	c := defaultClient()

	for _, o := range options {
		err := o(c)
		if err != nil {
			return nil, err
		}
	}

	if (len(c.options.servers) + len(c.options.secureServers)) < 1 {
		return nil, errors.New("no server provided, won't work ")
	}

	c.sendCh = make(chan Packet, c.options.sendChanSize)
	c.recvCh = make(chan *PublishPacket, c.options.recvChanSize)

	return c, nil
}

// AsyncClient mqtt client implementation
type AsyncClient struct {
	options *clientOptions      // client connection options
	msgCh   chan *message       // error channel
	sendCh  chan Packet         // pub channel for sending publish packet to server
	recvCh  chan *PublishPacket // recv channel for server pub receiving
	idGen   *idGenerator        // Packet id generator
	router  TopicRouter         // Topic router
	persist PersistMethod       // Persist method
	workers *sync.WaitGroup     // Workers (goroutines)
	log     *logger             // client logger

	// success/error handlers
	pubHandler     PubHandler
	subHandler     SubHandler
	unSubHandler   UnSubHandler
	netHandler     NetHandler
	persistHandler PersistHandler

	ctx  context.Context    // closure of this channel will signal all client worker to stop
	exit context.CancelFunc // called when client exit
}

// create a client with default options
func defaultClient() *AsyncClient {
	ctx, cancel := context.WithCancel(context.TODO())
	return &AsyncClient{
		options: &clientOptions{
			sendChanSize:     1,
			recvChanSize:     1,
			maxDelay:         2 * time.Minute,
			firstDelay:       5 * time.Second,
			backOffFactor:    1.5,
			dialTimeout:      20 * time.Second,
			keepalive:        2 * time.Minute,
			keepaliveFactor:  1.5,
			protoVersion:     V311,
			protoCompromise:  false,
			defaultTlsConfig: &tls.Config{},
		},
		msgCh:   make(chan *message, 10),
		ctx:     ctx,
		exit:    cancel,
		router:  NewTextRouter(),
		idGen:   newIDGenerator(),
		workers: &sync.WaitGroup{},
		persist: NonePersist,
	}
}

// Handle register subscription message route
func (c *AsyncClient) Handle(topic string, h TopicHandler) {
	if h != nil {
		c.log.d("HDL registered topic handler, topic =", topic)
		c.router.Handle(topic, h)
	}
}

// Connect to all designated server
func (c *AsyncClient) Connect(h ConnHandler) {
	c.log.d("CLI connect to server, handler =", h)

	for _, s := range c.options.servers {
		c.workers.Add(1)
		go c.connect(s, false, h)
	}

	for _, s := range c.options.secureServers {
		c.workers.Add(1)
		go c.connect(s, true, h)
	}

	c.workers.Add(2)
	go c.handleTopicMsg()
	go c.handleMsg()
}

// Publish message(s) to topic(s), one to one
func (c *AsyncClient) Publish(msg ...*PublishPacket) {
	if c.isClosing() {
		return
	}

	for _, m := range msg {
		if m == nil {
			continue
		}

		p := m
		if p.Qos > Qos2 {
			p.Qos = Qos2
		}

		if p.Qos != Qos0 {
			if p.PacketID == 0 {
				p.PacketID = c.idGen.next(p)
				if err := c.persist.Store(sendKey(p.PacketID), p); err != nil {
					notifyPersistMsg(c.msgCh, err)
				}
			}
		}
		c.sendCh <- p
	}
}

// Subscribe topic(s)
func (c *AsyncClient) Subscribe(topics ...*Topic) {
	if c.isClosing() {
		return
	}

	c.log.d("CLI subscribe, topic(s) =", topics)

	s := &SubscribePacket{Topics: topics}
	s.PacketID = c.idGen.next(s)

	c.sendCh <- s
}

// UnSubscribe topic(s)
func (c *AsyncClient) UnSubscribe(topics ...string) {
	if c.isClosing() {
		return
	}

	c.log.d("CLI unsubscribe topic(s) =", topics)

	u := &UnSubPacket{TopicNames: topics}
	u.PacketID = c.idGen.next(u)

	c.sendCh <- u
}

// Wait will wait for all connection to exit
func (c *AsyncClient) Wait() {
	if c.isClosing() {
		return
	}

	c.log.i("CLI wait for all workers")
	c.workers.Wait()
}

// Destroy will disconnect form all server
// If force is true, then close connection without sending a DisConnPacket
func (c *AsyncClient) Destroy(force bool) {
	c.log.d("CLI destroying client with force =", force)
	if force {
		c.exit()
	} else {
		c.sendCh <- &DisConnPacket{}
	}
}

// HandlePub register handler for pub error
func (c *AsyncClient) HandlePub(h PubHandler) {
	c.log.d("CLI registered pub handler")
	c.pubHandler = h
}

// HandleSub register handler for extra sub info
func (c *AsyncClient) HandleSub(h SubHandler) {
	c.log.d("CLI registered sub handler")
	c.subHandler = h
}

// HandleUnSub register handler for unsubscribe error
func (c *AsyncClient) HandleUnSub(h UnSubHandler) {
	c.log.d("CLI registered unsubscribe handler")
	c.unSubHandler = h
}

// HandleNet register handler for net error
func (c *AsyncClient) HandleNet(h NetHandler) {
	c.log.d("CLI registered net handler")
	c.netHandler = h
}

// HandlePersist register handler for net error
func (c *AsyncClient) HandlePersist(h PersistHandler) {
	c.log.d("CLI registered persist handler")
	c.persistHandler = h
}

// connect to one server and start mqtt logic
func (c *AsyncClient) connect(server string, secure bool, h ConnHandler) {
	defer c.workers.Done()

	// Number of failures since the last successful connection.
	nfail := 0

	for !c.isClosing() {
		tlsConfig := c.options.tlsConfig
		if secure {
			tlsConfig = c.options.defaultTlsConfig
		}

		if connImpl, err := c.tryConnect(server, tlsConfig); err != nil {
			nfail++
			c.log.e("CLI connect failed, err =", err, "server =", server, "failure count =", nfail)
			if h != nil {
				code := byte(math.MaxUint8)
				if conerr, ok := err.(connAckError); ok {
					code = byte(conerr)
				}
				go h(server, code, err)
			}
		} else {
			nfail = 0
			c.log.i("CLI connected to server =", server)
			if h != nil {
				go h(server, CodeSuccess, nil)
			}

			// login success, start mqtt logic
			connImpl.logic()
		}

		if c.isClosing() || !c.options.autoReconnect {
			return
		}

		// reconnect delay
		var delay time.Duration
		if nfail > 0 {
			delay = time.Duration(float64(c.options.firstDelay) * math.Pow(c.options.backOffFactor, float64(nfail-1)))
			if delay > c.options.maxDelay {
				delay = c.options.maxDelay
			}
		}
		c.log.e("CLI reconnecting to server =", server, "delay =", delay)

		select {
		case <-c.ctx.Done():
		case <-time.After(delay):
		}
	}
}

func (c *AsyncClient) tryConnect(server string, tlsConfig *tls.Config) (*clientConn, error) {
	// Enforce timeout for establishing connection.
	dialCtx, cancel := context.WithTimeout(c.ctx, c.options.dialTimeout)
	defer cancel()

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(dialCtx, "tcp", server)
	if err != nil {
		return nil, err
	}

	if tlsConfig != nil {
		// Perform TLS handshake.
		tlsConn := tls.Client(conn, tlsConfig)
		if err := honorContext(dialCtx, c.workers, tlsConn.Handshake); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tlsConn
	}

	connImpl := newClientConn(c.options.protoVersion, c, server, conn)

	c.workers.Add(2)
	go connImpl.handleSend()
	go connImpl.handleRecv()

	connImpl.send(&ConnPacket{
		Username:     c.options.username,
		Password:     c.options.password,
		ClientID:     c.options.clientID,
		CleanSession: c.options.cleanSession,
		IsWill:       c.options.isWill,
		WillQos:      c.options.willQos,
		WillTopic:    c.options.willTopic,
		WillMessage:  c.options.willPayload,
		WillRetain:   c.options.willRetain,
		Keepalive:    uint16(c.options.keepalive / time.Second),
	})

	if err := connImpl.waitForConnAck(dialCtx); err != nil {
		connImpl.exit()
		conn.Close()
		return nil, err
	}

	return connImpl, nil

}

func (c *AsyncClient) isClosing() bool {
	select {
	case <-c.ctx.Done():
		return true
	default:
		return false
	}
}

func (c *AsyncClient) handleTopicMsg() {
	defer c.workers.Done()

	for {
		select {
		case <-c.ctx.Done():
			return
		case pkt, more := <-c.recvCh:
			if !more {
				return
			}

			c.router.Dispatch(pkt)
		}
	}
}

func (c *AsyncClient) handleMsg() {
	defer c.workers.Done()

	for {
		select {
		case <-c.ctx.Done():
			return
		case m, more := <-c.msgCh:
			if !more {
				return
			}

			switch m.what {
			case pubMsg:
				if c.pubHandler != nil {
					c.pubHandler(m.msg, m.err)
				}
			case subMsg:
				if c.subHandler != nil {
					c.subHandler(m.obj.([]*Topic), m.err)
				}
			case unSubMsg:
				if c.unSubHandler != nil {
					c.unSubHandler(m.obj.([]string), m.err)
				}
			case netMsg:
				if c.netHandler != nil {
					c.netHandler(m.msg, m.err)
				}
			case persistMsg:
				if c.persistHandler != nil {
					c.persistHandler(m.err)
				}
			}
		}
	}
}
