package ckriprwc

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gologme/log"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv6"

	iwt "github.com/Arceliar/ironwood/types"
	"github.com/RiV-chain/RiVPN/src/config"

	"github.com/RiV-chain/RiV-mesh/src/address"
	"github.com/RiV-chain/RiV-mesh/src/core"
)

const keyStoreTimeout = 2 * time.Minute

// Out-of-band packet types
const (
	typeKeyDummy = iota // nolint:deadcode,varcheck
	typeKeyLookup
	typeKeyResponse
)

type keyArray [ed25519.PublicKeySize]byte

type keyStore struct {
	core         *core.Core
	log          *log.Logger
	ckr          *cryptokey
	address      address.Address
	subnet       address.Subnet
	mutex        sync.Mutex
	keyToInfo    map[keyArray]*keyInfo
	addrToInfo   map[address.Address]*keyInfo
	addrBuffer   map[address.Address]*buffer
	subnetToInfo map[address.Subnet]*keyInfo
	subnetBuffer map[address.Subnet]*buffer
	mtu          uint64
}

type keyInfo struct {
	key     keyArray
	address address.Address
	subnet  address.Subnet
	timeout *time.Timer // From calling a time.AfterFunc to do cleanup
}

type buffer struct {
	packet  []byte
	timeout *time.Timer
}

func (k *keyStore) init(c *core.Core, cfg *config.NodeConfig, log *log.Logger) {
	k.core = c
	k.log = log
	k.ckr = &cryptokey{
		config: &cfg.TunnelRoutingConfig,
		log:    log,
	}
	if err := k.ckr.configure(); err != nil {
		panic(err)
	}
	k.address = *address.AddrForKey(k.core.PublicKey())
	k.subnet = *address.SubnetForKey(k.core.PublicKey())
	if err := k.core.SetOutOfBandHandler(k.oobHandler); err != nil {
		err = fmt.Errorf("tun.core.SetOutOfBandHander: %w", err)
		panic(err)
	}
	k.keyToInfo = make(map[keyArray]*keyInfo)
	k.addrToInfo = make(map[address.Address]*keyInfo)
	k.addrBuffer = make(map[address.Address]*buffer)
	k.subnetToInfo = make(map[address.Subnet]*keyInfo)
	k.subnetBuffer = make(map[address.Subnet]*buffer)
	k.mtu = 1280 // Default to something safe, expect user to set this
}

func (k *keyStore) sendToAddress(addr address.Address, bs []byte) {
	k.mutex.Lock()
	if info := k.addrToInfo[addr]; info != nil {
		k.resetTimeout(info)
		k.mutex.Unlock()
		_, _ = k.core.WriteTo(bs, iwt.Addr(info.key[:]))
	} else {
		var buf *buffer
		if buf = k.addrBuffer[addr]; buf == nil {
			buf = new(buffer)
			k.addrBuffer[addr] = buf
		}
		msg := append([]byte(nil), bs...)
		buf.packet = msg
		if buf.timeout != nil {
			buf.timeout.Stop()
		}
		buf.timeout = time.AfterFunc(keyStoreTimeout, func() {
			k.mutex.Lock()
			defer k.mutex.Unlock()
			if nbuf := k.addrBuffer[addr]; nbuf == buf {
				delete(k.addrBuffer, addr)
			}
		})
		k.mutex.Unlock()
		k.sendKeyLookup(addr.GetKey())
	}
}

func (k *keyStore) sendToSubnet(subnet address.Subnet, bs []byte) {
	k.mutex.Lock()
	if info := k.subnetToInfo[subnet]; info != nil {
		k.resetTimeout(info)
		k.mutex.Unlock()
		_, _ = k.core.WriteTo(bs, iwt.Addr(info.key[:]))
	} else {
		var buf *buffer
		if buf = k.subnetBuffer[subnet]; buf == nil {
			buf = new(buffer)
			k.subnetBuffer[subnet] = buf
		}
		msg := append([]byte(nil), bs...)
		buf.packet = msg
		if buf.timeout != nil {
			buf.timeout.Stop()
		}
		buf.timeout = time.AfterFunc(keyStoreTimeout, func() {
			k.mutex.Lock()
			defer k.mutex.Unlock()
			if nbuf := k.subnetBuffer[subnet]; nbuf == buf {
				delete(k.subnetBuffer, subnet)
			}
		})
		k.mutex.Unlock()
		k.sendKeyLookup(subnet.GetKey())
	}
}

func (k *keyStore) update(key ed25519.PublicKey) *keyInfo {
	k.mutex.Lock()
	var kArray keyArray
	copy(kArray[:], key)
	var info *keyInfo
	if info = k.keyToInfo[kArray]; info == nil {
		info = new(keyInfo)
		info.key = kArray
		info.address = *address.AddrForKey(ed25519.PublicKey(info.key[:]))
		info.subnet = *address.SubnetForKey(ed25519.PublicKey(info.key[:]))
		k.keyToInfo[info.key] = info
		k.addrToInfo[info.address] = info
		k.subnetToInfo[info.subnet] = info
		k.resetTimeout(info)
		k.mutex.Unlock()
		if buf := k.addrBuffer[info.address]; buf != nil {
			_, _ = k.core.WriteTo(buf.packet, iwt.Addr(info.key[:]))
			delete(k.addrBuffer, info.address)
		}
		if buf := k.subnetBuffer[info.subnet]; buf != nil {
			_, _ = k.core.WriteTo(buf.packet, iwt.Addr(info.key[:]))
			delete(k.subnetBuffer, info.subnet)
		}
	} else {
		k.resetTimeout(info)
		k.mutex.Unlock()
	}
	return info
}

func (k *keyStore) resetTimeout(info *keyInfo) {
	if info.timeout != nil {
		info.timeout.Stop()
	}
	info.timeout = time.AfterFunc(keyStoreTimeout, func() {
		k.mutex.Lock()
		defer k.mutex.Unlock()
		if nfo := k.keyToInfo[info.key]; nfo == info {
			delete(k.keyToInfo, info.key)
		}
		if nfo := k.addrToInfo[info.address]; nfo == info {
			delete(k.addrToInfo, info.address)
		}
		if nfo := k.subnetToInfo[info.subnet]; nfo == info {
			delete(k.subnetToInfo, info.subnet)
		}
	})
}

func (k *keyStore) oobHandler(fromKey, toKey ed25519.PublicKey, data []byte) {
	if len(data) != 1+ed25519.SignatureSize {
		return
	}
	sig := data[1:]
	switch data[0] {
	case typeKeyLookup:
		snet := *address.SubnetForKey(toKey)
		if snet == k.subnet && ed25519.Verify(fromKey, toKey[:], sig) {
			// This is looking for at least our subnet (possibly our address)
			// Send a response
			k.sendKeyResponse(fromKey)
		}
	case typeKeyResponse:
		// TODO keep a list of something to match against...
		// Ignore the response if it doesn't match anything of interest...
		if ed25519.Verify(fromKey, toKey[:], sig) {
			k.update(fromKey)
		}
	}
}

func (k *keyStore) sendKeyLookup(partial ed25519.PublicKey) {
	sig := ed25519.Sign(k.core.PrivateKey(), partial[:])
	bs := append([]byte{typeKeyLookup}, sig...)
	_ = k.core.SendOutOfBand(partial, bs)
}

func (k *keyStore) sendKeyResponse(dest ed25519.PublicKey) {
	sig := ed25519.Sign(k.core.PrivateKey(), dest[:])
	bs := append([]byte{typeKeyResponse}, sig...)
	_ = k.core.SendOutOfBand(dest, bs)
}

func (k *keyStore) readPC(p []byte) (int, error) {
	buf := make([]byte, k.core.MTU(), 65535)
	for {
		bs := buf
		n, from, err := k.core.ReadFrom(bs)
		if err != nil {
			return n, err
		}
		if n == 0 {
			continue
		}
		bs = bs[:n]
		if len(bs) == 0 {
			continue
		}
		ip4 := bs[0]&0xf0 == 0x40
		ip6 := bs[0]&0xf0 == 0x60
		if !ip4 && !ip6 {
			continue // not IPv6
		}
		if ip6 && len(bs) < 40 {
			continue
		}
		k.mutex.Lock()
		mtu := int(k.mtu)
		k.mutex.Unlock()
		if len(bs) > mtu {
			if ip6 {
				// Using bs would make it leak off the stack, so copy to buf
				buf := make([]byte, 40)
				copy(buf, bs)
				ptb := &icmp.PacketTooBig{
					MTU:  mtu,
					Data: buf[:40],
				}
				if packet, err := CreateICMPv6(buf[8:24], buf[24:40], ipv6.ICMPTypePacketTooBig, 0, ptb); err == nil {
					_, _ = k.writePC(packet)
				}
			}
			continue
		}
		var srcAddr address.Address
		var srcSubnet address.Subnet
		var addrlen int
		switch {
		case ip4:
			copy(srcAddr[:], bs[12:16])
			addrlen = 4
		case ip6:
			copy(srcAddr[:], bs[8:24])
			addrlen = 16
		}
		info := k.update(ed25519.PublicKey(from.(iwt.Addr)))
		if srcAddr != info.address && srcSubnet != info.subnet {
			// check if it's a CKR source instead
			if _, err := k.ckr.getPublicKeyForAddress(srcAddr, addrlen); err != nil {
				continue
			}
		}
		n = copy(p, bs)
		return n, nil
	}
}

func (k *keyStore) writePC(bs []byte) (int, error) {
	ip4 := bs[0]&0xf0 == 0x40
	ip6 := bs[0]&0xf0 == 0x60
	if !ip4 && !ip6 {
		return 0, errors.New("not an IP packet")
	}
	if ip6 && len(bs) < 40 {
		strErr := fmt.Sprint("undersized IPv6 packet, length: ", len(bs))
		return 0, errors.New(strErr)
	}
	var dstAddr address.Address
	var dstSubnet address.Subnet
	var addrlen int
	switch {
	case ip4:
		copy(dstAddr[:], bs[16:20])
		addrlen = 4
	case ip6:
		copy(dstAddr[:], bs[24:40])
		copy(dstSubnet[:], bs[24:40])
		addrlen = 16
	}
	if key, err := k.ckr.getPublicKeyForAddress(dstAddr, addrlen); err == nil {
		_, _ = k.core.WriteTo(bs, iwt.Addr(key))
	} else if dstAddr.IsValid() {
		k.sendToAddress(dstAddr, bs)
	} else if dstSubnet.IsValid() {
		k.sendToSubnet(dstSubnet, bs)
	} else {
		return 0, errors.New("invalid destination address")
	}
	return len(bs), nil
}

// Exported API

func (k *keyStore) MaxMTU() uint64 {
	return k.core.MTU()
}

func (k *keyStore) SetMTU(mtu uint64) {
	if mtu > k.MaxMTU() {
		mtu = k.MaxMTU()
	}
	if mtu < 1280 {
		mtu = 1280
	}
	k.mutex.Lock()
	k.mtu = mtu
	k.mutex.Unlock()
}

func (k *keyStore) MTU() uint64 {
	k.mutex.Lock()
	mtu := k.mtu
	k.mutex.Unlock()
	return mtu
}

type ReadWriteCloser struct {
	keyStore
}

func NewReadWriteCloser(c *core.Core, cfg *config.NodeConfig, log *log.Logger) *ReadWriteCloser {
	rwc := new(ReadWriteCloser)
	rwc.init(c, cfg, log)
	return rwc
}

func (rwc *ReadWriteCloser) Address() address.Address {
	return rwc.address
}

func (rwc *ReadWriteCloser) Subnet() address.Subnet {
	return rwc.subnet
}

func (rwc *ReadWriteCloser) Read(p []byte) (n int, err error) {
	return rwc.readPC(p)
}

func (rwc *ReadWriteCloser) Write(p []byte) (n int, err error) {
	return rwc.writePC(p)
}

func (rwc *ReadWriteCloser) Close() error {
	err := rwc.core.Close()
	rwc.core.Stop()
	return err
}
