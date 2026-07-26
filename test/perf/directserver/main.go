// directserver is the protocol-matched baseline server for the B4 performance
// harness. It serves deterministic payloads over either one long-lived
// TLS/TCP connection or edge-opened streams on one long-lived QUIC connection.
// The only agent-opened QUIC stream is the session control stream, matching
// production endpoint roles. It contains no beamd framing or reverse proxy.
package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	quic "github.com/quic-go/quic-go"
)

const (
	alpn              = "beamd-perf-direct/1"
	planMagic         = "B4P1"
	planSize          = 17
	maxPayload        = 128 << 20
	maxPlanOperations = 10_000
)

type patternReader struct {
	pos int64
	n   int64
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.pos >= r.n {
		return 0, io.EOF
	}
	n := int64(len(p))
	if remaining := r.n - r.pos; remaining < n {
		n = remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = byte((r.pos + i) % 251)
	}
	r.pos += n
	return int(n), nil
}

func verifyPattern(r io.Reader, n int64) error {
	buf := make([]byte, 128*1024)
	var offset int64
	for offset < n {
		want := int64(len(buf))
		if remaining := n - offset; remaining < want {
			want = remaining
		}
		got, err := io.ReadFull(r, buf[:want])
		if err != nil {
			return err
		}
		for i := 0; i < got; i++ {
			if buf[i] != byte((offset+int64(i))%251) {
				return fmt.Errorf("payload mismatch at offset %d", offset+int64(i))
			}
		}
		offset += int64(got)
	}
	return nil
}

type measurementPlan struct {
	direction  byte
	size       int64
	operations int
}

func readPlan(rw io.ReadWriter) (measurementPlan, error) {
	var encoded [planSize]byte
	if _, err := io.ReadFull(rw, encoded[:]); err != nil {
		return measurementPlan{}, err
	}
	size := int64(binary.BigEndian.Uint64(encoded[5:13]))
	operations := int(binary.BigEndian.Uint32(encoded[13:17]))
	plan := measurementPlan{
		direction:  encoded[4],
		size:       size,
		operations: operations,
	}
	valid := string(encoded[:4]) == planMagic &&
		(plan.direction == 'D' || plan.direction == 'U') &&
		size > 0 && size <= maxPayload &&
		operations > 0 && operations <= maxPlanOperations
	status := byte(0)
	if valid {
		status = 1
	}
	if _, err := rw.Write([]byte{status}); err != nil {
		return measurementPlan{}, err
	}
	if !valid {
		return measurementPlan{}, errors.New("invalid measurement plan")
	}
	return plan, nil
}

func serveRequest(rw io.ReadWriter, plan measurementPlan) error {
	var header [9]byte
	header[0] = plan.direction
	binary.BigEndian.PutUint64(header[1:], uint64(plan.size))
	if _, err := rw.Write(header[:]); err != nil {
		return err
	}

	switch plan.direction {
	case 'D':
		status := byte(1)
		verifyErr := verifyPattern(rw, plan.size)
		if verifyErr != nil {
			status = 0
		}
		if _, err := rw.Write([]byte{status}); err != nil {
			return err
		}
		if verifyErr != nil {
			return verifyErr
		}
		return nil
	case 'U':
		if _, err := io.CopyN(rw, &patternReader{n: plan.size}, plan.size); err != nil {
			return err
		}
		var ack [1]byte
		if _, err := io.ReadFull(rw, ack[:]); err != nil {
			return err
		}
		if ack[0] != 1 {
			return errors.New("upload checksum rejected by client")
		}
		return nil
	default:
		return fmt.Errorf("unknown operation %q", header[0])
	}
}

func tlsConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		NextProtos:   []string{alpn},
	}, nil
}

func serveTCP(ctx context.Context, addr string, tlsConf *tls.Config) error {
	raw, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer raw.Close()
	listener := tls.NewListener(raw, tlsConf)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer conn.Close()
			plan, err := readPlan(conn)
			if err == nil {
				for range plan.operations {
					if err = serveRequest(conn, plan); err != nil {
						break
					}
				}
			}
			if err == nil {
				// Keep the warmed connection alive through the client's last
				// sampled stream. EOF is the client's post-report teardown.
				_, err = io.Copy(io.Discard, conn)
			}
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("TCP fixture connection closed: %v", err)
			}
		}()
	}
}

func quicConfig() *quic.Config {
	return &quic.Config{
		InitialStreamReceiveWindow:     4 << 20,
		MaxStreamReceiveWindow:         16 << 20,
		InitialConnectionReceiveWindow: 16 << 20,
		MaxConnectionReceiveWindow:     64 << 20,
		MaxIncomingStreams:             1,
		MaxIncomingUniStreams:          -1,
		HandshakeIdleTimeout:           10 * time.Second,
		MaxIdleTimeout:                 75 * time.Second,
		KeepAlivePeriod:                0,
	}
}

func serveQUIC(ctx context.Context, addr string, tlsConf *tls.Config) error {
	listener, err := quic.ListenAddr(addr, tlsConf, quicConfig())
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			defer conn.CloseWithError(0, "")
			control, err := conn.AcceptStream(ctx)
			if err != nil {
				return
			}
			defer control.Close()
			plan, err := readPlan(control)
			if err != nil {
				log.Printf("QUIC fixture control stream closed: %v", err)
				return
			}
			for range plan.operations {
				stream, openErr := conn.OpenStreamSync(ctx)
				if openErr != nil {
					err = openErr
					break
				}
				err = serveRequest(stream, plan)
				closeErr := stream.Close()
				if err == nil {
					_, err = io.Copy(io.Discard, stream)
				}
				if err == nil {
					err = closeErr
				}
				if err != nil {
					break
				}
			}
			if err == nil {
				// The agent-opened control stream remains alive for the full
				// session, exactly like production. The client closes it only
				// after all sampled edge-opened data streams have drained.
				_, err = io.Copy(io.Discard, control)
			}
			if err != nil && !errors.Is(err, net.ErrClosed) {
				log.Printf("QUIC fixture data stream closed: %v", err)
			}
		}()
	}
}

func main() {
	transport := flag.String("transport", "", "tcp or quic")
	addr := flag.String("addr", "", "listen address")
	certFile := flag.String("cert", "", "PEM certificate")
	keyFile := flag.String("key", "", "PEM private key")
	flag.Parse()

	if (*transport != "tcp" && *transport != "quic") ||
		*addr == "" || *certFile == "" || *keyFile == "" {
		fmt.Fprintln(os.Stderr, "--transport tcp|quic, --addr, --cert, and --key are required")
		os.Exit(2)
	}
	tlsConf, err := tlsConfig(*certFile, *keyFile)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("direct %s fixture listening on %s", *transport, *addr)
	if *transport == "tcp" {
		err = serveTCP(ctx, *addr, tlsConf)
	} else {
		err = serveQUIC(ctx, *addr, tlsConf)
	}
	if err != nil {
		log.Fatal(err)
	}
}
