package main

import (
	"bytes"
	"encoding/base32"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
	"www.bamsoftware.com/git/dnstt.git/dns"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

const (
	idleTimeout = 10 * time.Minute
	responseTTL = 60
)

// A base32 encoding without padding.
var base32Encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// handleStream bidirectionally connects a client stream with the ORPort.
func handleStream(stream *smux.Stream, upstream *net.TCPAddr) error {
	conn, err := net.DialTCP("tcp", nil, upstream)
	if err != nil {
		return err
	}
	defer conn.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := io.Copy(stream, conn)
		if err != nil {
			log.Printf("copy stream←upstream: %v\n", err)
		}
		stream.Close()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := io.Copy(conn, stream)
		if err != nil {
			log.Printf("copy upstream←stream: %v\n", err)
		}
		conn.Close()
	}()
	wg.Wait()

	return nil
}

// acceptStreams layers an smux.Session on a KCP connection and awaits streams
// on it. It passes each stream to handleStream.
func acceptStreams(conn *kcp.UDPSession, upstream *net.TCPAddr) error {
	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 2
	smuxConfig.KeepAliveTimeout = idleTimeout
	sess, err := smux.Server(conn, smuxConfig)
	if err != nil {
		return err
	}

	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			return err
		}
		go func() {
			defer stream.Close()
			err := handleStream(stream, upstream)
			if err != nil {
				log.Printf("handleStream: %v\n", err)
			}
		}()
	}
}

// acceptSessions listens for incoming KCP connections and passes them to
// acceptStreams.
func acceptSessions(ln *kcp.Listener, upstream *net.TCPAddr) error {
	for {
		conn, err := ln.AcceptKCP()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			return err
		}
		// Permit coalescing the payloads of consecutive sends.
		conn.SetStreamMode(true)
		// Disable the dynamic congestion window (limit only by the
		// maximum of local and remote static windows).
		conn.SetNoDelay(
			0, // default nodelay
			0, // default interval
			0, // default resend
			1, // nc=1 => congestion window off
		)
		// Set the maximum transmission unit.
		mtu := dnsMessageCapacity()
		if mtu < 80 {
			// This value doesn't depend on any configuration values, so it
			// should never be too small.
			panic("too little space for downstream payload")
		}
		conn.SetMtu(mtu)
		go func() {
			defer conn.Close()
			err := acceptStreams(conn, upstream)
			if err != nil {
				log.Printf("acceptStreams: %v\n", err)
			}
		}()
	}
}

func dnsMessageCapacity() int {
	longName, err := dns.NewName([][]byte{
		bytes.Repeat([]byte{'a'}, 63),
		bytes.Repeat([]byte{'b'}, 63),
		bytes.Repeat([]byte{'c'}, 63),
		bytes.Repeat([]byte{'d'}, 61),
	})
	if err != nil {
		panic(err)
	}
	message := dns.Message{
		Question: []dns.Question{
			dns.Question{Name: longName},
		},
		Answer: []dns.RR{
			dns.RR{Name: longName},
		},
	}
	builder, err := message.WireFormat()
	if err != nil {
		panic(err)
	}
	return (512 - len(builder)) * 255 / 256
}

func nextPacket(r *bytes.Reader) ([]byte, error) {
	eof := func(err error) error {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}

	for {
		prefix, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if prefix >= 224 {
			paddingLen := prefix - 224
			_, err := io.CopyN(ioutil.Discard, r, int64(paddingLen))
			if err != nil {
				return nil, eof(err)
			}
			continue
		}
		p := make([]byte, int(prefix))
		_, err = io.ReadFull(r, p)
		return p, eof(err)
	}
}

func responseFor(query *dns.Message, domain dns.Name, ttConn *turbotunnel.QueuePacketConn) *dns.Message {
	resp := &dns.Message{
		ID:       query.ID,
		Flags:    0x8400, // QR = 1, AA = 1, RCODE = no error
		Question: query.Question,
	}

	if query.Flags&0x8000 != 0 {
		// QR != 0, this is not a query. Don't even send a response.
		return nil
	}
	if query.Flags&0x7800 != 0 {
		// We don't support OPCODE != QUERY.
		resp.Flags |= dns.RcodeNotImplemented
		return resp
	}

	if len(query.Question) != 1 {
		// There must be exactly one question.
		resp.Flags |= dns.RcodeFormatError
		return resp
	}
	question := query.Question[0]
	if question.Type != dns.RRTypeTXT {
		// We only support QTYPE == TXT. Send an empty response.
		return resp
	}

	prefix, ok := question.Name.TrimSuffix(domain)
	if !ok {
		// Not a name we are authoritative for.
		resp.Flags |= dns.RcodeNameError
		return resp
	}

	encoded := bytes.ToUpper(bytes.Join(prefix, nil))
	payload := make([]byte, base32Encoding.DecodedLen(len(encoded)))
	n, err := base32Encoding.Decode(payload, encoded)
	if err != nil {
		// Base32 error, make like the name doesn't exist.
		resp.Flags |= dns.RcodeNameError
		return resp
	}
	payload = payload[:n]

	// Now extract the ClientID.
	var clientID turbotunnel.ClientID
	n = copy(clientID[:], payload)
	if n < len(clientID) {
		// Payload is not long enough to contain a ClientID.
		resp.Flags |= dns.RcodeNameError
		return resp
	}

	// Discard padding and pull out the packets contained in the payload.
	buf := bytes.NewReader(payload[len(clientID):])
	for {
		p, err := nextPacket(buf)
		// Feed the incoming packet to KCP.
		ttConn.QueueIncoming(p, clientID)
		if err != nil {
			break
		}
	}

	// Send a downstream packet if any is available.
	// TODO: can bundle multiple packets here.
	select {
	case p := <-ttConn.OutgoingQueue(clientID):
		resp.Answer = append(resp.Answer, dns.RR{
			Name: question.Name,
			Type: dns.RRTypeTXT,
			TTL:  responseTTL,
			Data: dns.EncodeRDataTXT(p),
		})
	default:
	}

	return resp
}

func handle(p []byte, addr net.Addr, dnsConn net.PacketConn, domain dns.Name, ttConn *turbotunnel.QueuePacketConn) error {
	query, err := dns.MessageFromWireFormat(p)
	if err != nil {
		return fmt.Errorf("parsing DNS query: %v", err)
	}

	resp := responseFor(&query, domain, ttConn)
	if resp != nil {
		buf, err := resp.WireFormat()
		if err != nil {
			return err
		}
		_, err = dnsConn.WriteTo(buf, addr)
		if err != nil {
			return err
		}
	}

	return nil
}

func loop(dnsConn net.PacketConn, domain dns.Name, ttConn *turbotunnel.QueuePacketConn) error {
	type taggedPacket struct {
		P    []byte
		Addr net.Addr
	}

	handleChan := make(chan taggedPacket, 64)
	defer close(handleChan)
	go func() {
		for tp := range handleChan {
			p := tp.P
			addr := tp.Addr
			err := handle(p, addr, dnsConn, domain, ttConn)
			if err != nil {
				log.Printf("handle from %v: %v\n", addr, err)
			}
		}
	}()

	for {
		// One byte longer than we want, to check for truncation.
		var buf [513]byte
		n, addr, err := dnsConn.ReadFrom(buf[:])
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				log.Printf("ReadFrom temporary error: %v", err)
				continue
			}
			return err
		}
		if n == len(buf) {
			log.Printf("ReadFrom: truncated packet")
			continue
		}
		// Copy the packet data into its own buffer.
		p := make([]byte, n)
		copy(p, buf[:n])
		select {
		case handleChan <- taggedPacket{p, addr}:
		default:
			// Drop incoming packets if channel is full.
		}
	}
}

type dummyAddr struct{}

func (addr dummyAddr) Network() string { return "dummy" }
func (addr dummyAddr) String() string  { return "dummy" }

func run(domain dns.Name, upstream net.Addr, udpAddr string) error {
	// Start up the virtual PacketConn for turbotunnel.
	ttConn := turbotunnel.NewQueuePacketConn(dummyAddr{}, idleTimeout*2)
	ln, err := kcp.ServeConn(nil, 0, 0, ttConn)
	if err != nil {
		return fmt.Errorf("opening KCP listener: %v", err)
	}
	defer ln.Close()
	go func() {
		err := acceptSessions(ln, upstream.(*net.TCPAddr))
		if err != nil {
			log.Printf("acceptSessions: %v\n", err)
		}
	}()

	var wg sync.WaitGroup

	if udpAddr != "" {
		dnsConn, err := net.ListenPacket("udp", udpAddr)
		if err != nil {
			return fmt.Errorf("opening UDP listener: %v", err)
		}
		wg.Add(1)
		go func() {
			defer dnsConn.Close()
			defer wg.Done()
			err := loop(dnsConn, domain, ttConn)
			if err != nil {
				log.Printf("error in UDP loop: %v\n", err)
			}
		}()
	}

	wg.Wait()
	return nil
}

func main() {
	var udpAddr string

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s -udp ADDR DOMAIN UPSTREAMADDR\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.StringVar(&udpAddr, "udp", "", "UDP address to listen on")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.LUTC)

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(1)
	}
	domain, err := dns.ParseName(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid domain %+q: %v\n", flag.Arg(0), err)
		os.Exit(1)
	}
	upstream, err := net.ResolveTCPAddr("tcp", flag.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot resolve %+q: %v\n", flag.Arg(1), err)
		os.Exit(1)
	}

	err = run(domain, upstream, udpAddr)
	if err != nil {
		log.Fatal(err)
	}
}
