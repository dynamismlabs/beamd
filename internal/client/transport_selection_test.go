package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/proto"
	"github.com/dynamismlabs/beamd/internal/tunnel"
)

func TestTransportDefaultIsForcedTCP(t *testing.T) {
	var opts Options
	if err := opts.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if opts.Transport != "tcp" {
		t.Fatalf("default transport = %q, want tcp", opts.Transport)
	}

	c := newSelectionTestClient("unused", opts)
	assertCandidateOrder(t, c.candidateOrder(true), tunnel.KindYamux)
	assertCandidateOrder(t, c.candidateOrder(false), tunnel.KindYamux)
}

func TestTransportOptionValidation(t *testing.T) {
	for _, transport := range []string{"tcp", "quic", "auto"} {
		t.Run(transport, func(t *testing.T) {
			opts := Options{Transport: transport}
			if err := opts.applyDefaults(); err != nil {
				t.Fatalf("applyDefaults(%q): %v", transport, err)
			}
		})
	}

	opts := Options{Transport: "udp"}
	if err := opts.applyDefaults(); err == nil {
		t.Fatal("applyDefaults accepted unsupported transport")
	}
}

func TestAutoCandidateOrderAndQUICReprobe(t *testing.T) {
	opts := Options{Transport: "auto"}
	if err := opts.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	c := newSelectionTestClient("unused", opts)

	assertCandidateOrder(t, c.candidateOrder(true), tunnel.KindQUIC, tunnel.KindYamux)

	c.lastSuccessful = tunnel.KindQUIC
	assertCandidateOrder(t, c.candidateOrder(false), tunnel.KindQUIC, tunnel.KindYamux)

	c.lastSuccessful = tunnel.KindYamux
	c.tcpFallbackAt = time.Now()
	assertCandidateOrder(t, c.candidateOrder(false), tunnel.KindYamux, tunnel.KindQUIC)

	c.tcpFallbackAt = time.Now().Add(-quicReprobeInterval - time.Second)
	assertCandidateOrder(t, c.candidateOrder(false), tunnel.KindQUIC, tunnel.KindYamux)
}

func TestForcedTransportCandidateOrderNeverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want tunnel.Kind
	}{
		{mode: "tcp", want: tunnel.KindYamux},
		{mode: "quic", want: tunnel.KindQUIC},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			opts := Options{Transport: tc.mode}
			if err := opts.applyDefaults(); err != nil {
				t.Fatal(err)
			}
			c := newSelectionTestClient("unused", opts)
			assertCandidateOrder(t, c.candidateOrder(true), tc.want)
			assertCandidateOrder(t, c.candidateOrder(false), tc.want)
		})
	}
}

func TestQUICFallbackClassification(t *testing.T) {
	tests := []struct {
		name     string
		category tunnel.DialFailureCategory
		reason   string
		eligible bool
	}{
		{name: "network", category: tunnel.DialNetwork, reason: "network", eligible: true},
		{name: "timeout", category: tunnel.DialTimeout, reason: "timeout", eligible: true},
		{name: "handshake", category: tunnel.DialHandshake, reason: "handshake", eligible: true},
		{name: "certificate protocol or auth terminal", category: tunnel.DialTerminal, reason: "terminal", eligible: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dialErr := &tunnel.DialFailure{
				Category: tc.category,
				Address:  "127.0.0.1:443",
				Err:      errors.New("candidate failed"),
			}
			err := classifyDialFailure(context.Background(), tunnel.KindQUIC, dialErr)
			var candidate *candidateError
			if !errors.As(err, &candidate) {
				t.Fatalf("classifyDialFailure returned %T, want *candidateError", err)
			}
			if candidate.reason != tc.reason || candidate.eligible != tc.eligible {
				t.Fatalf("classification = reason %q eligible %v, want %q/%v",
					candidate.reason, candidate.eligible, tc.reason, tc.eligible)
			}
		})
	}
}

func TestUnknownAndCanceledFailuresAreTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
	}{
		{name: "unknown", ctx: context.Background(), err: errors.New("unclassified")},
		{name: "caller canceled", ctx: canceledContext(), err: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyAvailabilityFailure(tc.ctx, tc.err)
			var candidate *candidateError
			if !errors.As(err, &candidate) {
				t.Fatalf("classification returned %T, want *candidateError", err)
			}
			if candidate.eligible {
				t.Fatalf("failure was fallback eligible: %+v", candidate)
			}
			if candidate.reason != "" {
				t.Fatalf("terminal failure reason = %q, want empty fixed fallback reason", candidate.reason)
			}
		})
	}
}

func TestAbruptSocketFailuresAreNetworkEligible(t *testing.T) {
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, net.ErrClosed} {
		classifiedErr := classifyAvailabilityFailure(context.Background(), err)
		var classified *candidateError
		if !errors.As(classifiedErr, &classified) {
			t.Fatalf("%v classification returned %T, want *candidateError", err, classifiedErr)
		}
		if !classified.eligible || classified.reason != "network" {
			t.Fatalf("%v classification = reason %q eligible %v, want network/true",
				err, classified.reason, classified.eligible)
		}
	}
}

func TestRemoteApplicationCloseDuringHelloIsAlwaysTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		code tunnel.ErrorCode
	}{
		{name: "normal", code: tunnel.CloseNormal},
		{name: "shutdown", code: tunnel.CloseShutdown},
		{name: "superseded", code: tunnel.CloseSuperseded},
		{name: "unknown", code: 0xff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := newSelectionTestSession(tunnel.KindQUIC, nil)
			transport.closed.Store(true)
			transport.setCloseInfo(tunnel.CloseInfo{
				Code:      tc.code,
				CodeValid: true,
				Remote:    true,
				Reason:    "explicit application close",
			})
			rejection := closedHelloRejection(context.Background(), transport)
			if rejection == nil {
				t.Fatalf("remote application close %#x was fallback eligible", tc.code)
			}
			if rejection.eligible {
				t.Fatalf("remote application close %#x was marked eligible", tc.code)
			}
		})
	}
}

func TestClosedHelloPublicationWaitUsesCandidateContext(t *testing.T) {
	transport := newSelectionTestSession(tunnel.KindQUIC, nil)
	transport.closed.Store(true)
	if rejection := closedHelloRejection(canceledContext(), transport); rejection != nil {
		t.Fatalf("canceled CloseInfo publication wait returned rejection: %v", rejection)
	}
}

func TestOuterContextCancellationPropagatesUnchanged(t *testing.T) {
	opts := Options{Transport: "auto"}
	if err := opts.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	// The malformed address makes the first candidate fail without opening a
	// socket. connectOnce must still return the authoritative outer error.
	c := newSelectionTestClient("not-a-host-port", opts)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.connectOnce(ctx, true)
	if err != context.Canceled {
		t.Fatalf("connectOnce error = %v, want the unchanged context.Canceled sentinel", err)
	}
}

func TestCandidateCleanupAbortsThenClosesAndJoins(t *testing.T) {
	events := make(chan string, 4)
	stream := newSelectionTestStream(events)
	transport := newSelectionTestSession(tunnel.KindQUIC, events)
	candidate := &session{transport: transport, control: stream}

	result := make(chan error, 1)
	go func() { result <- cleanupCandidate(candidate) }()

	if got := awaitEvent(t, events); got != "abort" {
		t.Fatalf("first cleanup event = %q, want abort", got)
	}
	if got := awaitEvent(t, events); got != "close" {
		t.Fatalf("second cleanup event = %q, want close", got)
	}
	select {
	case err := <-result:
		t.Fatalf("cleanup returned before Session.Done: %v", err)
	default:
	}

	transport.finish()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("cleanupCandidate: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup did not join Session.Done")
	}
}

func TestControlEOFClosesSessionBeforeControlSendHalf(t *testing.T) {
	events := make(chan string, 4)
	control := newSelectionTestStream(events)
	transport := newSelectionTestSession(tunnel.KindQUIC, events)
	s := &session{
		transport: transport,
		control:   control,
		br:        bufio.NewReader(control),
	}
	c := newSelectionTestClient("unused", Options{Transport: "quic"})

	c.readControl(s)
	if got := awaitEvent(t, events); got != "close" {
		t.Fatalf("first teardown event = %q, want session close", got)
	}
	if got := awaitEvent(t, events); got != "close_write" {
		t.Fatalf("second teardown event = %q, want control CloseWrite", got)
	}
}

func TestControlShutdownMessageClaimsShutdownBeforeEOF(t *testing.T) {
	local, peer := net.Pipe()
	control := newTrackedPipeStream(local)
	t.Cleanup(func() { _ = control.Close() })
	events := make(chan string, 1)
	transport := newSelectionTestSession(tunnel.KindQUIC, events)
	s := &session{
		transport: transport,
		control:   control,
		br:        bufio.NewReader(control),
	}
	c := newSelectionTestClient("unused", Options{Transport: "quic"})

	writeDone := make(chan error, 1)
	go func() {
		err := proto.Write(peer, &proto.Error{
			Type:    proto.TypeError,
			Code:    proto.CodeShutdown,
			Message: "planned restart",
		})
		if closeErr := peer.Close(); err == nil {
			err = closeErr
		}
		writeDone <- err
	}()

	c.readControl(s)
	if err := <-writeDone; err != nil {
		t.Fatalf("write shutdown control message: %v", err)
	}
	if got := awaitEvent(t, events); got != "close" {
		t.Fatalf("teardown event = %q, want session close", got)
	}
	info := transport.CloseInfo()
	if !info.CodeValid || info.Code != tunnel.CloseShutdown || info.Reason != "planned restart" {
		t.Fatalf("CloseInfo = %+v, want authoritative shutdown message", info)
	}
	if !c.skipBackoff.Load() {
		t.Fatal("shutdown control message did not request immediate reconnect")
	}
}

func TestSuccessfulCandidateHonorsOuterCancellationBeforeInstall(t *testing.T) {
	events := make(chan string, 4)
	stream := newSelectionTestStream(events)
	transport := newSelectionTestSession(tunnel.KindQUIC, events)
	candidate := &session{transport: transport, control: stream}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	candidateCtx, cancelCandidate := context.WithTimeout(context.Background(), time.Hour)
	defer cancelCandidate()

	result := make(chan error, 1)
	go func() {
		result <- rejectCanceledSuccessfulCandidate(ctx, candidateCtx, candidate)
	}()
	if got := awaitEvent(t, events); got != "abort" {
		t.Fatalf("first cancellation event = %q, want abort", got)
	}
	if got := awaitEvent(t, events); got != "close" {
		t.Fatalf("second cancellation event = %q, want close", got)
	}
	select {
	case err := <-result:
		t.Fatalf("canceled successful candidate returned before Session.Done: %v", err)
	default:
	}

	transport.finish()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("error = %v, want unchanged context.Canceled sentinel", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled successful candidate did not join cleanup")
	}
}

func TestSuccessfulCandidateDeadlineCleansBeforeEligibleFallback(t *testing.T) {
	events := make(chan string, 4)
	stream := newSelectionTestStream(events)
	transport := newSelectionTestSession(tunnel.KindQUIC, events)
	candidate := &session{transport: transport, control: stream}
	candidateCtx, cancelCandidate := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelCandidate()

	result := make(chan error, 1)
	go func() {
		result <- rejectCanceledSuccessfulCandidate(context.Background(), candidateCtx, candidate)
	}()
	if got := awaitEvent(t, events); got != "abort" {
		t.Fatalf("first deadline event = %q, want abort", got)
	}
	if got := awaitEvent(t, events); got != "close" {
		t.Fatalf("second deadline event = %q, want close", got)
	}
	select {
	case err := <-result:
		t.Fatalf("expired successful candidate returned before Session.Done: %v", err)
	default:
	}

	transport.finish()
	select {
	case err := <-result:
		var classified *candidateError
		if !errors.As(err, &classified) {
			t.Fatalf("error = %T %v, want *candidateError", err, err)
		}
		if !classified.eligible || classified.reason != "timeout" {
			t.Fatalf("classification = reason %q eligible %v, want timeout/true",
				classified.reason, classified.eligible)
		}
	case <-time.After(time.Second):
		t.Fatal("expired successful candidate did not join cleanup")
	}
}

func TestCandidateCleanupTimeoutMakesFailureTerminal(t *testing.T) {
	events := make(chan string, 4)
	stream := newSelectionTestStream(events)
	transport := newSelectionTestSession(tunnel.KindQUIC, events)
	candidate := &session{transport: transport, control: stream}
	availabilityFailure := &candidateError{
		reason:   "network",
		eligible: true,
		err:      errors.New("network unavailable"),
	}

	err := failCandidate(candidate, availabilityFailure)
	var classified *candidateError
	if !errors.As(err, &classified) {
		t.Fatalf("failCandidate returned %T, want *candidateError", err)
	}
	if classified.eligible {
		t.Fatal("cleanup timeout left the candidate fallback eligible")
	}
	if !strings.Contains(err.Error(), "cleanup exceeded") {
		t.Fatalf("failCandidate error = %v, want cleanup timeout", err)
	}
	if got := awaitEvent(t, events); got != "abort" {
		t.Fatalf("first cleanup event = %q, want abort", got)
	}
	if got := awaitEvent(t, events); got != "close" {
		t.Fatalf("second cleanup event = %q, want close", got)
	}
	transport.finish()
}

func TestCandidateCleanupRechecksRacingRemoteApplicationClose(t *testing.T) {
	events := make(chan string, 4)
	control := newSelectionTestStream(events)
	transport := newSelectionTestSession(tunnel.KindQUIC, events)
	transport.closed.Store(true)
	candidate := &session{transport: transport, control: control}
	timeoutFailure := &candidateError{
		reason:   "timeout",
		eligible: true,
		err:      context.DeadlineExceeded,
	}

	result := make(chan error, 1)
	go func() { result <- failCandidate(candidate, timeoutFailure) }()
	if got := awaitEvent(t, events); got != "abort" {
		t.Fatalf("first cleanup event = %q, want abort", got)
	}
	if got := awaitEvent(t, events); got != "close" {
		t.Fatalf("second cleanup event = %q, want close", got)
	}

	// Publish the peer close only after the original availability decision,
	// exactly where the race occurs in a real QUIC session.
	transport.setCloseInfo(tunnel.CloseInfo{
		Code:      tunnel.CloseAuth,
		CodeValid: true,
		Remote:    true,
		Reason:    "authentication rejected",
	})
	transport.finish()

	select {
	case err := <-result:
		var classified *candidateError
		if !errors.As(err, &classified) {
			t.Fatalf("failCandidate error = %T %v, want *candidateError", err, err)
		}
		if classified.eligible || classified.reason != "" {
			t.Fatalf("racing remote close classification = reason %q eligible %v, want terminal",
				classified.reason, classified.eligible)
		}
		if !strings.Contains(err.Error(), proto.CodeBadToken) {
			t.Fatalf("terminal error = %v, want stable %q rejection", err, proto.CodeBadToken)
		}
	case <-time.After(time.Second):
		t.Fatal("failCandidate did not finish after Session.Done")
	}
}

func TestCandidateCleanupRechecksRacingQUICTransportClose(t *testing.T) {
	tests := []struct {
		name       string
		closeInfo  tunnel.CloseInfo
		wantReason string
		eligible   bool
	}{
		{
			name: "protocol is terminal",
			closeInfo: tunnel.CloseInfo{
				Remote: true,
				Reason: "protocol",
				Cause:  errors.New("transport protocol violation"),
			},
		},
		{
			name: "unknown is terminal",
			closeInfo: tunnel.CloseInfo{
				Remote: true,
				Reason: "other",
				Cause:  errors.New("unclassified transport failure"),
			},
		},
		{
			name: "network remains eligible",
			closeInfo: tunnel.CloseInfo{
				Remote: true,
				Reason: "network",
				Cause:  errors.New("path unavailable"),
			},
			wantReason: "network",
			eligible:   true,
		},
		{
			name: "idle remains timeout eligible",
			closeInfo: tunnel.CloseInfo{
				Remote: true,
				Reason: "idle",
				Cause:  errors.New("idle timeout"),
			},
			wantReason: "timeout",
			eligible:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := make(chan string, 4)
			control := newSelectionTestStream(events)
			transport := newSelectionTestSession(tunnel.KindQUIC, events)
			transport.closed.Store(true)
			candidate := &session{transport: transport, control: control}
			availabilityFailure := &candidateError{
				reason:   "network",
				eligible: true,
				err:      errors.New("initial availability failure"),
			}

			result := make(chan error, 1)
			go func() { result <- failCandidate(candidate, availabilityFailure) }()
			if got := awaitEvent(t, events); got != "abort" {
				t.Fatalf("first cleanup event = %q, want abort", got)
			}
			if got := awaitEvent(t, events); got != "close" {
				t.Fatalf("second cleanup event = %q, want close", got)
			}
			transport.setCloseInfo(tc.closeInfo)
			transport.finish()

			select {
			case err := <-result:
				var classified *candidateError
				if !errors.As(err, &classified) {
					t.Fatalf("failCandidate error = %T %v, want *candidateError", err, err)
				}
				if classified.eligible != tc.eligible || classified.reason != tc.wantReason {
					t.Fatalf("classification = reason %q eligible %v, want %q/%v",
						classified.reason, classified.eligible, tc.wantReason, tc.eligible)
				}
			case <-time.After(time.Second):
				t.Fatal("failCandidate did not finish after Session.Done")
			}
		})
	}
}

func TestQUICResolvedAddressRetryIncludesHelloStage(t *testing.T) {
	firstControl := newSelectionTestStream(nil)
	firstTransport := newSelectionTestSession(tunnel.KindQUIC, nil)
	firstTransport.openStream = firstControl
	firstTransport.setCloseInfo(tunnel.CloseInfo{
		Remote: true,
		Reason: "network",
		Cause:  errors.New("first path closed during hello"),
	})
	firstControl.readHook = firstTransport.finish

	clientSide, serverSide := net.Pipe()
	secondControl := newTrackedPipeStream(clientSide)
	secondTransport := newSelectionTestSession(tunnel.KindQUIC, nil)
	secondTransport.openStream = secondControl
	serverErr := make(chan error, 1)
	go func() {
		defer serverSide.Close()
		typ, _, err := proto.Read(bufio.NewReader(serverSide))
		if err != nil {
			serverErr <- err
			return
		}
		if typ != proto.TypeHello {
			serverErr <- fmt.Errorf("message type = %q, want hello", typ)
			return
		}
		serverErr <- proto.Write(serverSide, &proto.HelloOK{
			Type:         proto.TypeHelloOK,
			Slug:         "turing",
			BaseDomain:   "example.test",
			ProtoVersion: proto.ProtoVersion,
		})
	}()

	dialer := &selectionQUICCandidateDialer{
		serverName: "edge.example.test",
		addresses:  []string{"192.0.2.1:443", "192.0.2.2:443"},
		sessions: map[string]tunnel.Session{
			"192.0.2.1:443": firstTransport,
			"192.0.2.2:443": secondTransport,
		},
	}
	dialer.beforeDial = func(address string) {
		if address != "192.0.2.2:443" {
			return
		}
		select {
		case <-firstTransport.Done():
		default:
			t.Fatal("second address dialed before the first session joined Done")
		}
		select {
		case <-firstControl.Done():
		default:
			t.Fatal("second address dialed before the first control stream was cleaned")
		}
	}
	opts := Options{Transport: "quic", InsecureSkipVerify: true}
	if err := opts.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	c := newSelectionTestClient("edge.example.test:443", opts)
	c.quicDialer = dialer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	candidate, hello, err := c.connectCandidate(ctx, tunnel.KindQUIC)
	if err != nil {
		t.Fatalf("connectCandidate: %v", err)
	}
	if hello.Slug != "turing" || candidate.transport != secondTransport {
		t.Fatalf("selected candidate = %+v hello=%+v, want second address", candidate, hello)
	}
	if got := dialer.dialedAddresses(); !slices.Equal(got, dialer.addresses) {
		t.Fatalf("dialed addresses = %v, want %v", got, dialer.addresses)
	}
	select {
	case <-firstControl.Done():
	default:
		t.Fatal("first address control stream was not cleaned before retry")
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("second address hello server: %v", err)
	}
	secondControl.Abort(tunnel.StreamCanceled)
	secondTransport.finish()
}

func TestAutoCooldownTriesQUICWhenTCPPathCloses(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen TCP: %v", err)
	}
	defer tcp.Close()
	tcpClosed := make(chan struct{})
	go func() {
		conn, acceptErr := tcp.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
		close(tcpClosed)
	}()

	clientSide, serverSide := net.Pipe()
	control := newTrackedPipeStream(clientSide)
	quicTransport := newSelectionTestSession(tunnel.KindQUIC, nil)
	quicTransport.openStream = control
	releaseServer := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		defer serverSide.Close()
		typ, _, readErr := proto.Read(bufio.NewReader(serverSide))
		if readErr != nil {
			serverErr <- readErr
			return
		}
		if typ != proto.TypeHello {
			serverErr <- fmt.Errorf("message type = %q, want hello", typ)
			return
		}
		if writeErr := proto.Write(serverSide, &proto.HelloOK{
			Type:         proto.TypeHelloOK,
			Slug:         "turing",
			BaseDomain:   "example.test",
			ProtoVersion: proto.ProtoVersion,
		}); writeErr != nil {
			serverErr <- writeErr
			return
		}
		serverErr <- nil
		<-releaseServer
	}()

	dialer := &selectionQUICCandidateDialer{
		serverName: "edge.example.test",
		addresses:  []string{"192.0.2.2:443"},
		sessions: map[string]tunnel.Session{
			"192.0.2.2:443": quicTransport,
		},
	}
	opts := Options{Transport: "auto", InsecureSkipVerify: true, HeartbeatInterval: time.Hour}
	if err := opts.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	c := newSelectionTestClient(tcp.Addr().String(), opts)
	c.quicDialer = dialer
	c.slug = "turing"
	c.baseDomain = "example.test"
	c.lastSuccessful = tunnel.KindYamux
	c.tcpFallbackAt = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := c.connectOnce(ctx, false); err != nil {
		t.Fatalf("connectOnce: %v", err)
	}
	if got := c.Transport(); got != tunnel.KindQUIC {
		t.Fatalf("selected transport = %q, want QUIC after TCP became unavailable", got)
	}
	select {
	case <-tcpClosed:
	case <-time.After(time.Second):
		t.Fatal("TCP-first candidate was not attempted")
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("QUIC hello server: %v", err)
	}

	close(releaseServer)
	_ = c.Close()
	control.Abort(tunnel.StreamCanceled)
	quicTransport.finish()
}

func TestShutdownCloseInfoReconnectsWithoutInitialBackoff(t *testing.T) {
	opts := Options{
		Transport:        "tcp",
		ReconnectInitial: time.Hour,
		ReconnectMax:     time.Hour,
	}
	c := newSelectionTestClient("not-a-host-port", opts)
	transport := newSelectionTestSession(tunnel.KindQUIC, nil)
	transport.setCloseInfo(tunnel.CloseInfo{
		Code:      tunnel.CloseShutdown,
		CodeValid: true,
		Remote:    true,
		Reason:    "edge shutdown",
	})
	c.sess = &session{transport: transport}

	go c.manage()
	transport.finish()
	t.Cleanup(func() { _ = c.Close() })

	deadline := time.After(time.Second)
	for c.reconnectCount.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("shutdown CloseInfo did not trigger an immediate reconnect")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestRegisterSkipsDeadSessionUntilReplacement(t *testing.T) {
	opts := Options{Transport: "tcp", RegisterTimeout: time.Second}
	c := newSelectionTestClient("unused", opts)
	deadTransport := newSelectionTestSession(tunnel.KindYamux, nil)
	deadTransport.finish()
	c.sess = &session{
		transport: deadTransport,
		control:   newSelectionTestStream(nil),
	}
	replacementTransport := newSelectionTestSession(tunnel.KindYamux, nil)
	replacement := &session{
		transport: replacementTransport,
		control:   newSelectionTestStream(nil),
	}
	t.Cleanup(func() {
		replacementTransport.finish()
		_ = c.Close()
	})

	type result struct {
		url string
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		url, err := c.registerNow("api", 3000)
		resultCh <- result{url: url, err: err}
	}()

	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	c.sess = replacement
	c.mu.Unlock()
	pending := awaitPendingRegister(t, replacement, "api")
	pending.ch <- controlReply{registered: &proto.Registered{
		Type: proto.TypeRegistered,
		Name: "api",
		URL:  "https://api.example.test",
	}}

	select {
	case got := <-resultCh:
		if got.err != nil || got.url != "https://api.example.test" {
			t.Fatalf("register result = %q, %v", got.url, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("register did not use the replacement session")
	}
}

func TestRegisterRetriesWhenSessionDiesAwaitingReply(t *testing.T) {
	opts := Options{Transport: "tcp", RegisterTimeout: time.Second}
	c := newSelectionTestClient("unused", opts)
	firstTransport := newSelectionTestSession(tunnel.KindYamux, nil)
	first := &session{
		transport: firstTransport,
		control:   newSelectionTestStream(nil),
	}
	secondTransport := newSelectionTestSession(tunnel.KindYamux, nil)
	second := &session{
		transport: secondTransport,
		control:   newSelectionTestStream(nil),
	}
	c.sess = first
	t.Cleanup(func() {
		firstTransport.finish()
		secondTransport.finish()
		_ = c.Close()
	})

	type result struct {
		url string
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		url, err := c.registerNow("api", 3000)
		resultCh <- result{url: url, err: err}
	}()
	_ = awaitPendingRegister(t, first, "api")
	firstTransport.finish()
	c.mu.Lock()
	c.sess = second
	c.mu.Unlock()
	pending := awaitPendingRegister(t, second, "api")
	pending.ch <- controlReply{registered: &proto.Registered{
		Type: proto.TypeRegistered,
		Name: "api",
		URL:  "https://api.example.test",
	}}

	select {
	case got := <-resultCh:
		if got.err != nil || got.url != "https://api.example.test" {
			t.Fatalf("register result = %q, %v", got.url, got.err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("dead session held register until the full timeout")
	}
}

func TestTransportDiagnostics(t *testing.T) {
	opts := Options{Transport: "auto"}
	if err := opts.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	transport := newSelectionTestSession(tunnel.KindQUIC, nil)
	c := newSelectionTestClient("unused", opts)
	c.sess = &session{transport: transport}
	c.fallbackCount.Store(2)
	c.lastFallback.Store("handshake")
	c.reconnectCount.Store(3)
	c.lastClose.Store("network")

	if got := c.Transport(); got != tunnel.KindQUIC {
		t.Fatalf("Transport = %q, want quic", got)
	}
	got := c.Diagnostics()
	if got.ConfiguredTransport != "auto" ||
		got.FallbackCount != 2 ||
		got.LastFallbackReason != "handshake" ||
		got.ReconnectCount != 3 ||
		got.LastCloseReason != "network" {
		t.Fatalf("Diagnostics = %+v", got)
	}

	transport.finish()
	if got := c.Transport(); got != "" {
		t.Fatalf("Transport after session close = %q, want empty", got)
	}
}

func TestReconnectUpdatesDiagnosticsAndRetainsTCP(t *testing.T) {
	var round atomic.Int32
	fe := newFakeEdge(t, func(_ tunnel.Stream, sess tunnel.Session, _ *bufio.Reader, _ proto.Hello) {
		if round.Add(1) == 1 {
			return
		}
		<-sess.Done()
	})
	c := dialFake(t, fe, Options{
		Transport:        "tcp",
		ReconnectInitial: 10 * time.Millisecond,
		ReconnectMax:     20 * time.Millisecond,
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fe.connCount() >= 2 && c.IsHealthy() && c.Diagnostics().ReconnectCount >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fe.connCount() < 2 {
		t.Fatalf("connections = %d, want a reconnect", fe.connCount())
	}
	if got := c.Transport(); got != tunnel.KindYamux {
		t.Fatalf("transport after reconnect = %q, want tcp", got)
	}
	diag := c.Diagnostics()
	if diag.ReconnectCount < 1 {
		t.Fatalf("ReconnectCount = %d, want at least 1", diag.ReconnectCount)
	}
	switch diag.LastCloseReason {
	case "normal", "shutdown", "idle", "protocol", "network", "other":
	default:
		t.Fatalf("LastCloseReason = %q, want a fixed category", diag.LastCloseReason)
	}
}

func TestAutoUnavailableQUICFallsBackToTCP(t *testing.T) {
	fe := newFakeEdge(t, func(_ tunnel.Stream, sess tunnel.Session, _ *bufio.Reader, _ proto.Hello) {
		<-sess.Done()
	})
	c := dialFake(t, fe, Options{Transport: "auto"})

	if got := c.Transport(); got != tunnel.KindYamux {
		t.Fatalf("selected transport = %q, want tcp", got)
	}
	diag := c.Diagnostics()
	if diag.ConfiguredTransport != "auto" || diag.FallbackCount != 1 {
		t.Fatalf("diagnostics after fallback = %+v", diag)
	}
	switch diag.LastFallbackReason {
	case "network", "timeout", "handshake":
	default:
		t.Fatalf("fallback reason = %q, want fixed availability category", diag.LastFallbackReason)
	}
}

func TestAutoDoesNotFallbackOnTerminalQUICControlErrors(t *testing.T) {
	tests := []struct {
		name      string
		reply     func(tunnel.Stream) error
		wantInErr string
	}{
		{
			name: "bad token",
			reply: func(stream tunnel.Stream) error {
				return proto.Write(stream, &proto.Error{
					Type: proto.TypeError, Code: proto.CodeBadToken, Message: "rejected",
				})
			},
			wantInErr: proto.CodeBadToken,
		},
		{
			name: "scope or session capacity rejection",
			reply: func(stream tunnel.Stream) error {
				return proto.Write(stream, &proto.Error{
					Type: proto.TypeError, Code: proto.CodeOverLimit, Message: "rejected",
				})
			},
			wantInErr: proto.CodeOverLimit,
		},
		{
			name: "bad version error",
			reply: func(stream tunnel.Stream) error {
				return proto.Write(stream, &proto.Error{
					Type: proto.TypeError, Code: proto.CodeBadVersion, Message: "upgrade together",
				})
			},
			wantInErr: proto.CodeBadVersion,
		},
		{
			name: "hello ok version mismatch",
			reply: func(stream tunnel.Stream) error {
				return proto.Write(stream, &proto.HelloOK{
					Type: proto.TypeHelloOK, Slug: "turing",
					BaseDomain: "test.example.com", ProtoVersion: proto.ProtoVersion + 1,
				})
			},
			wantInErr: "does not match",
		},
		{
			name: "unexpected protocol reply",
			reply: func(stream tunnel.Stream) error {
				return proto.Write(stream, &proto.Heartbeat{Type: proto.TypeHeartbeat})
			},
			wantInErr: "expected hello_ok",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			edge := newTerminalDualEdge(t, tc.reply)
			opts := Options{
				Transport:          "auto",
				InsecureSkipVerify: true,
				HeartbeatInterval:  time.Hour,
			}
			if err := opts.applyDefaults(); err != nil {
				t.Fatal(err)
			}
			c := newSelectionTestClient(edge.addr, opts)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := c.connectOnce(ctx, true)
			if err == nil || !strings.Contains(err.Error(), tc.wantInErr) {
				t.Fatalf("connectOnce error = %v, want substring %q", err, tc.wantInErr)
			}
			if got := edge.tcpAccepts.Load(); got != 0 {
				t.Fatalf("terminal QUIC failure attempted TCP %d time(s)", got)
			}
			if got := c.Diagnostics().FallbackCount; got != 0 {
				t.Fatalf("terminal QUIC failure incremented fallback count to %d", got)
			}
		})
	}
}

func TestAutoDoesNotFallbackOnQUICCertificateFailure(t *testing.T) {
	edge := newTerminalDualEdge(t, func(tunnel.Stream) error { return nil })
	opts := Options{Transport: "auto", HeartbeatInterval: time.Hour}
	if err := opts.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	c := newSelectionTestClient(edge.addr, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.connectOnce(ctx, true)
	if err == nil {
		t.Fatal("connectOnce unexpectedly trusted the test certificate")
	}
	if got := edge.tcpAccepts.Load(); got != 0 {
		t.Fatalf("certificate failure attempted TCP %d time(s)", got)
	}
	if got := c.Diagnostics().FallbackCount; got != 0 {
		t.Fatalf("certificate failure incremented fallback count to %d", got)
	}
}

func TestHandleStreamRejectsInvalidOrUnboundedPrefix(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "missing newline", payload: "api"},
		{name: "over 63 byte label", payload: strings.Repeat("a", 64) + "\n"},
		{name: "invalid label", payload: "Bad_Name\n"},
		{name: "unknown label", payload: "missing\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runRejectedPrefix(t, tc.payload)
		})
	}
}

func newSelectionTestClient(serverAddr string, opts Options) *Client {
	return &Client{
		serverAddr:   serverAddr,
		token:        "tok",
		opts:         opts,
		intended:     make(map[string]int),
		closed:       make(chan struct{}),
		handlerSlots: make(chan struct{}, maxStreamHandlers),
		quicDialer:   tunnel.NewQUICDialer(),
	}
}

func assertCandidateOrder(t *testing.T, got []tunnel.Kind, want ...tunnel.Kind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("candidate order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate order = %v, want %v", got, want)
		}
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type selectionTestSession struct {
	kind       tunnel.Kind
	done       chan struct{}
	events     chan<- string
	openStream tunnel.Stream
	openErr    error
	closeOnce  sync.Once
	closed     atomic.Bool
	infoMu     sync.Mutex
	closeInfo  tunnel.CloseInfo
}

func newSelectionTestSession(kind tunnel.Kind, events chan<- string) *selectionTestSession {
	return &selectionTestSession{kind: kind, done: make(chan struct{}), events: events}
}

func (s *selectionTestSession) Kind() tunnel.Kind { return s.kind }
func (s *selectionTestSession) OpenStream(context.Context) (tunnel.Stream, error) {
	if s.openStream != nil || s.openErr != nil {
		return s.openStream, s.openErr
	}
	return nil, errors.New("not implemented")
}
func (s *selectionTestSession) AcceptStream(context.Context) (tunnel.Stream, error) {
	return nil, errors.New("not implemented")
}
func (s *selectionTestSession) Done() <-chan struct{} { return s.done }
func (s *selectionTestSession) IsClosed() bool        { return s.closed.Load() }
func (s *selectionTestSession) CloseInfo() tunnel.CloseInfo {
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	if s.closeInfo.CodeValid || s.closeInfo.Cause != nil || s.closeInfo.Reason != "" {
		return s.closeInfo
	}
	return tunnel.CloseInfo{CodeValid: true, Code: tunnel.CloseNormal, Reason: "test"}
}
func (s *selectionTestSession) setCloseInfo(info tunnel.CloseInfo) {
	s.infoMu.Lock()
	s.closeInfo = info
	s.infoMu.Unlock()
}
func (s *selectionTestSession) CloseWithError(code tunnel.ErrorCode, reason string) error {
	s.infoMu.Lock()
	if !s.closeInfo.CodeValid && s.closeInfo.Cause == nil && s.closeInfo.Reason == "" {
		s.closeInfo = tunnel.CloseInfo{
			Code:      code,
			CodeValid: true,
			Reason:    reason,
		}
	}
	s.infoMu.Unlock()
	if s.events != nil {
		s.events <- "close"
	}
	s.closed.Store(true)
	return nil
}
func (s *selectionTestSession) LocalAddr() net.Addr  { return selectionTestAddr("local") }
func (s *selectionTestSession) RemoteAddr() net.Addr { return selectionTestAddr("remote") }
func (s *selectionTestSession) finish() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.done)
	})
}

type selectionTestStream struct {
	events    chan<- string
	done      chan struct{}
	abortOnce sync.Once
	readOnce  sync.Once
	readHook  func()
}

func newSelectionTestStream(events chan<- string) *selectionTestStream {
	return &selectionTestStream{events: events, done: make(chan struct{})}
}

func (s *selectionTestStream) Read([]byte) (int, error) {
	s.readOnce.Do(func() {
		if s.readHook != nil {
			s.readHook()
		}
	})
	return 0, io.EOF
}
func (s *selectionTestStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *selectionTestStream) Close() error                { return nil }
func (s *selectionTestStream) CloseWrite() error {
	if s.events != nil {
		s.events <- "close_write"
	}
	return nil
}
func (s *selectionTestStream) LocalAddr() net.Addr              { return selectionTestAddr("local") }
func (s *selectionTestStream) RemoteAddr() net.Addr             { return selectionTestAddr("remote") }
func (s *selectionTestStream) SetDeadline(time.Time) error      { return nil }
func (s *selectionTestStream) SetReadDeadline(time.Time) error  { return nil }
func (s *selectionTestStream) SetWriteDeadline(time.Time) error { return nil }
func (s *selectionTestStream) Done() <-chan struct{}            { return s.done }
func (s *selectionTestStream) Abort(tunnel.ErrorCode) {
	s.abortOnce.Do(func() {
		if s.events != nil {
			s.events <- "abort"
		}
		close(s.done)
	})
}

type selectionTestAddr string

func (a selectionTestAddr) Network() string { return "test" }
func (a selectionTestAddr) String() string  { return string(a) }

func awaitEvent(t *testing.T, events <-chan string) string {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cleanup event")
		return ""
	}
}

func awaitPendingRegister(t *testing.T, s *session, name string) *pendingRegister {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.pendingMu.Lock()
		pending := s.pending
		s.pendingMu.Unlock()
		if pending != nil && pending.name == name {
			return pending
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session never began register %q", name)
	return nil
}

type selectionQUICCandidateDialer struct {
	serverName string
	addresses  []string
	sessions   map[string]tunnel.Session
	beforeDial func(string)

	mu     sync.Mutex
	dialed []string
}

func (d *selectionQUICCandidateDialer) Resolve(
	context.Context,
	string,
) (string, []string, error) {
	return d.serverName, append([]string(nil), d.addresses...), nil
}

func (d *selectionQUICCandidateDialer) DialResolved(
	_ context.Context,
	address string,
	_ string,
	_ *tls.Config,
) (tunnel.Session, error) {
	if d.beforeDial != nil {
		d.beforeDial(address)
	}
	d.mu.Lock()
	d.dialed = append(d.dialed, address)
	d.mu.Unlock()
	session := d.sessions[address]
	if session == nil {
		return nil, &tunnel.DialFailure{
			Category: tunnel.DialNetwork,
			Address:  address,
			Err:      errors.New("no scripted session"),
		}
	}
	return session, nil
}

func (d *selectionQUICCandidateDialer) dialedAddresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

type terminalDualEdge struct {
	addr       string
	tcp        net.Listener
	quic       tunnel.Listener
	quicIO     io.Closer
	reply      func(tunnel.Stream) error
	tcpAccepts atomic.Int32
}

func newTerminalDualEdge(t *testing.T, reply func(tunnel.Stream) error) *terminalDualEdge {
	t.Helper()
	tlsConfig := fakeEdgeTLS(t)
	keyDir := t.TempDir()
	var (
		rawTCP       net.Listener
		quicListener tunnel.Listener
		quicIO       io.Closer
		err          error
	)
	for range 20 {
		rawTCP, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen TCP: %v", err)
		}
		quicListener, quicIO, err = tunnel.ListenQUIC(rawTCP.Addr().String(), tlsConfig, keyDir, nil)
		if err == nil {
			break
		}
		_ = rawTCP.Close()
		rawTCP = nil
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("listen QUIC: %v", err)
		}
	}
	if err != nil {
		t.Fatalf("listen QUIC after retries: %v", err)
	}
	tcp := tls.NewListener(rawTCP, tlsConfig)
	edge := &terminalDualEdge{
		addr:   rawTCP.Addr().String(),
		tcp:    tcp,
		quic:   quicListener,
		quicIO: quicIO,
		reply:  reply,
	}
	go edge.acceptTCP()
	go edge.acceptQUIC()
	t.Cleanup(func() {
		_ = edge.tcp.Close()
		_ = edge.quic.Close()
		_ = edge.quicIO.Close()
	})
	return edge
}

func (e *terminalDualEdge) acceptTCP() {
	for {
		conn, err := e.tcp.Accept()
		if err != nil {
			return
		}
		e.tcpAccepts.Add(1)
		_ = conn.Close()
	}
}

func (e *terminalDualEdge) acceptQUIC() {
	for {
		sess, err := e.quic.Accept(context.Background())
		if err != nil {
			return
		}
		go e.handleQUIC(sess)
	}
}

func (e *terminalDualEdge) handleQUIC(sess tunnel.Session) {
	defer sess.CloseWithError(tunnel.CloseNormal, "test complete")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := sess.AcceptStream(ctx)
	if err != nil {
		return
	}
	defer stream.Close()
	typ, _, err := proto.Read(bufio.NewReader(stream))
	if err != nil || typ != proto.TypeHello {
		return
	}
	if e.reply != nil {
		_ = e.reply(stream)
	}
	select {
	case <-sess.Done():
	case <-ctx.Done():
	}
}

type trackedPipeStream struct {
	net.Conn
	aborted      chan struct{}
	done         chan struct{}
	abortOnce    sync.Once
	closeOnce    sync.Once
	deadlineMu   sync.Mutex
	readDeadline []time.Time
}

func newTrackedPipeStream(conn net.Conn) *trackedPipeStream {
	return &trackedPipeStream{
		Conn:    conn,
		aborted: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (s *trackedPipeStream) CloseWrite() error { return nil }
func (s *trackedPipeStream) Done() <-chan struct{} {
	return s.done
}
func (s *trackedPipeStream) Abort(tunnel.ErrorCode) {
	s.abortOnce.Do(func() {
		close(s.aborted)
		_ = s.Close()
	})
}
func (s *trackedPipeStream) Close() error {
	err := s.Conn.Close()
	s.closeOnce.Do(func() { close(s.done) })
	return err
}

func (s *trackedPipeStream) SetReadDeadline(deadline time.Time) error {
	s.deadlineMu.Lock()
	s.readDeadline = append(s.readDeadline, deadline)
	s.deadlineMu.Unlock()
	return s.Conn.SetReadDeadline(deadline)
}

func (s *trackedPipeStream) readDeadlines() []time.Time {
	s.deadlineMu.Lock()
	defer s.deadlineMu.Unlock()
	return append([]time.Time(nil), s.readDeadline...)
}

func runRejectedPrefix(t *testing.T, payload string) {
	t.Helper()
	local, peer := net.Pipe()
	stream := newTrackedPipeStream(local)
	transport := newSelectionTestSession(tunnel.KindYamux, nil)
	c := newSelectionTestClient("unused", Options{Transport: "tcp"})
	sess := &session{transport: transport}
	handled := make(chan struct{})
	go func() {
		c.handleStream(sess, stream)
		close(handled)
	}()
	writeDone := make(chan struct{})
	go func() {
		_, _ = io.WriteString(peer, payload)
		_ = peer.Close()
		close(writeDone)
	}()

	select {
	case <-stream.aborted:
	case <-time.After(time.Second):
		t.Fatal("invalid prefix did not abort stream")
	}
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("handleStream did not return after rejecting prefix")
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("prefix writer did not exit")
	}
	deadlines := stream.readDeadlines()
	if len(deadlines) == 0 || deadlines[0].IsZero() {
		t.Fatal("prefix read did not install a bounded setup deadline")
	}
	if newline := strings.IndexByte(payload, '\n'); newline >= 0 && newline < 63 {
		if len(deadlines) < 2 || !deadlines[len(deadlines)-1].IsZero() {
			t.Fatal("valid-length prefix read did not clear its setup deadline")
		}
	}
	transport.finish()
}

func TestDialLocalBackendUsesBoundedContext(t *testing.T) {
	if streamSetupTimeout != 5*time.Second {
		t.Fatalf("stream setup timeout = %s, want 5s", streamSetupTimeout)
	}
	const testTimeout = 20 * time.Millisecond
	started := time.Now()
	var gotAddress string
	var gotDeadline time.Time
	_, err := dialLocalBackend(testTimeout, 4321, func(
		ctx context.Context,
		network string,
		address string,
	) (net.Conn, error) {
		if network != "tcp" {
			t.Fatalf("network = %q, want tcp", network)
		}
		gotAddress = address
		gotDeadline, _ = ctx.Deadline()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dial error = %v, want context deadline exceeded", err)
	}
	if gotAddress != "127.0.0.1:4321" {
		t.Fatalf("dial address = %q, want loopback backend", gotAddress)
	}
	if gotDeadline.IsZero() {
		t.Fatal("backend dial context had no deadline")
	}
	if elapsed := gotDeadline.Sub(started); elapsed < testTimeout/2 || elapsed > 2*testTimeout {
		t.Fatalf("backend dial deadline after %s, want approximately %s", elapsed, testTimeout)
	}
}
