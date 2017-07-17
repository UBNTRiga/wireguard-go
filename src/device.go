package main

import (
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type Device struct {
	mtu       int32
	log       *Logger // collection of loggers for levels
	idCounter uint    // for assigning debug ids to peers
	fwMark    uint32
	pool      struct {
		// pools objects for reuse
		messageBuffers sync.Pool
	}
	net struct {
		// seperate for performance reasons
		mutex sync.RWMutex
		addr  *net.UDPAddr // UDP source address
		conn  *net.UDPConn // UDP "connection"
	}
	mutex        sync.RWMutex
	privateKey   NoisePrivateKey
	publicKey    NoisePublicKey
	routingTable RoutingTable
	indices      IndexTable
	queue        struct {
		encryption chan *QueueOutboundElement
		decryption chan *QueueInboundElement
		inbound    chan *QueueInboundElement
		handshake  chan QueueHandshakeElement
	}
	signal struct {
		stop chan struct{}
	}
	underLoad   int32 // used as an atomic bool
	ratelimiter Ratelimiter
	peers       map[NoisePublicKey]*Peer
	mac         MACStateDevice
}

func (device *Device) SetPrivateKey(sk NoisePrivateKey) {
	device.mutex.Lock()
	defer device.mutex.Unlock()

	// update key material

	device.privateKey = sk
	device.publicKey = sk.publicKey()
	device.mac.Init(device.publicKey)

	// do DH precomputations

	for _, peer := range device.peers {
		h := &peer.handshake
		h.mutex.Lock()
		h.precomputedStaticStatic = device.privateKey.sharedSecret(h.remoteStatic)
		h.mutex.Unlock()
	}
}

func (device *Device) GetMessageBuffer() *[MaxMessageSize]byte {
	return device.pool.messageBuffers.Get().(*[MaxMessageSize]byte)
}

func (device *Device) PutMessageBuffer(msg *[MaxMessageSize]byte) {
	device.pool.messageBuffers.Put(msg)
}

func NewDevice(tun TUNDevice, logLevel int) *Device {
	device := new(Device)

	device.mutex.Lock()
	defer device.mutex.Unlock()

	device.log = NewLogger(logLevel)
	device.peers = make(map[NoisePublicKey]*Peer)
	device.indices.Init()
	device.ratelimiter.Init()
	device.routingTable.Reset()

	// listen

	device.net.mutex.Lock()
	device.net.conn, _ = net.ListenUDP("udp", device.net.addr)
	addr := device.net.conn.LocalAddr()
	device.net.addr, _ = net.ResolveUDPAddr(addr.Network(), addr.String())
	device.net.mutex.Unlock()

	// setup pools

	device.pool.messageBuffers = sync.Pool{
		New: func() interface{} {
			return new([MaxMessageSize]byte)
		},
	}

	// create queues

	device.queue.handshake = make(chan QueueHandshakeElement, QueueHandshakeSize)
	device.queue.encryption = make(chan *QueueOutboundElement, QueueOutboundSize)
	device.queue.decryption = make(chan *QueueInboundElement, QueueInboundSize)
	device.queue.inbound = make(chan *QueueInboundElement, QueueInboundSize)

	// prepare signals

	device.signal.stop = make(chan struct{})

	// start workers

	for i := 0; i < runtime.NumCPU(); i += 1 {
		go device.RoutineEncryption()
		go device.RoutineDecryption()
		go device.RoutineHandshake()
	}

	go device.RoutineBusyMonitor()
	go device.RoutineMTUUpdater(tun)
	go device.RoutineWriteToTUN(tun)
	go device.RoutineReadFromTUN(tun)
	go device.RoutineReceiveIncomming()
	go device.ratelimiter.RoutineGarbageCollector(device.signal.stop)

	return device
}

func (device *Device) RoutineMTUUpdater(tun TUNDevice) {
	logError := device.log.Error
	for ; ; time.Sleep(5 * time.Second) {

		// load updated MTU

		mtu, err := tun.MTU()
		if err != nil {
			logError.Println("Failed to load updated MTU of device:", err)
			continue
		}

		// upper bound of mtu

		if mtu+MessageTransportSize > MaxMessageSize {
			mtu = MaxMessageSize - MessageTransportSize
		}
		atomic.StoreInt32(&device.mtu, int32(mtu))
	}
}

func (device *Device) LookupPeer(pk NoisePublicKey) *Peer {
	device.mutex.RLock()
	defer device.mutex.RUnlock()
	return device.peers[pk]
}

func (device *Device) RemovePeer(key NoisePublicKey) {
	device.mutex.Lock()
	defer device.mutex.Unlock()

	peer, ok := device.peers[key]
	if !ok {
		return
	}
	peer.mutex.Lock()
	device.routingTable.RemovePeer(peer)
	delete(device.peers, key)
	peer.Close()
}

func (device *Device) RemoveAllPeers() {
	device.mutex.Lock()
	defer device.mutex.Unlock()

	for key, peer := range device.peers {
		peer.mutex.Lock()
		delete(device.peers, key)
		peer.Close()
		peer.mutex.Unlock()
	}
}

func (device *Device) Close() {
	device.RemoveAllPeers()
	close(device.signal.stop)
}

func (device *Device) Wait() {
	<-device.signal.stop
}
