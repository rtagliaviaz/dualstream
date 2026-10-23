package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const sfuURL = "http://localhost:8080"

const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 10 * time.Second
)

// extIDs holds the negotiated extension IDs and MID for the current
// PeerConnection. Stored atomically and read by the forever-running UDP
// forwarders on every packet. When nil, forwarders drop packets.
type extIDs struct {
	midID, ridID uint8
	mid          string
}

type udpLayer struct {
	port  int
	rid   string
	track *webrtc.TrackLocalStaticRTP
}

func main() {
	api, err := newAPI()
	if err != nil {
		log.Fatalf("webrtc api: %v", err)
	}

	// Tracks are created once and reused across reconnects. A
	// TrackLocalStaticRTP can bind to a fresh RTPSender on each new PC.
	capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}
	mustTrack := func(rid string) *webrtc.TrackLocalStaticRTP {
		t, err := webrtc.NewTrackLocalStaticRTP(capability, "video", "broadcaster",
			webrtc.WithRTPStreamID(rid))
		if err != nil {
			log.Fatalf("track %s: %v", rid, err)
		}
		return t
	}

	layers := []udpLayer{
		{port: 5000, rid: "h", track: mustTrack("h")},
		{port: 5001, rid: "m", track: mustTrack("m")},
		{port: 5002, rid: "l", track: mustTrack("l")},
	}

	// UDP forwarders run forever. They write into the shared tracks;
	// whether a PC is consuming them depends on the active sender.
	var currentExt atomic.Pointer[extIDs]
	for _, l := range layers {
		l := l
		go listenAndForward(l.port, l.rid, l.track, &currentExt)
	}

	// ---- Reconnect loop ----------------------------------------------
	backoff := initialBackoff
	for {
		pc, sender, tr, err := connectAndPublish(api, layers)
		if err != nil {
			log.Printf("bridge: connect failed: %v (retry in %s)", err, backoff)
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = initialBackoff

		ext, err := discoverExtensions(sender, tr)
		if err != nil {
			log.Printf("bridge: extension discovery failed: %v", err)
			_ = pc.Close()
			continue
		}
		currentExt.Store(ext)
		log.Printf("bridge: connected (MID=%d RID=%d mid=%q)",
			ext.midID, ext.ridID, ext.mid)

		waitForClose(pc)
		log.Printf("bridge: PC closed, reconnecting…")
		currentExt.Store(nil)
		_ = pc.Close()
	}
}

func newAPI() (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureSimulcastExtensionHeaders(m); err != nil {
		return nil, err
	}
	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(i),
	), nil
}

// connectAndPublish creates a fresh PC, wires the three tracks into a
// single simulcast transceiver, negotiates with the SFU, and blocks
// until the PC is connected (or fails).
func connectAndPublish(api *webrtc.API, layers []udpLayer) (
	*webrtc.PeerConnection, *webrtc.RTPSender, *webrtc.RTPTransceiver, error,
) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, nil, nil, err
	}

	transceiver, err := pc.AddTransceiverFromTrack(layers[0].track,
		webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendonly,
			SendEncodings: []webrtc.RTPEncodingParameters{
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: layers[0].rid}},
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: layers[1].rid}},
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: layers[2].rid}},
			},
		},
	)
	if err != nil {
		_ = pc.Close()
		return nil, nil, nil, err
	}

	sender := transceiver.Sender()
	for _, l := range layers[1:] {
		if err := sender.AddEncoding(l.track); err != nil {
			_ = pc.Close()
			return nil, nil, nil, err
		}
	}

	// Drain RTCP on this sender so Pion doesn't block on the receive path.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, nil, nil, err
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return nil, nil, nil, err
	}
	<-gatherDone

	log.Println("bridge: sending offer to SFU…")
	answer, err := postSDP(sfuURL+"/broadcast", pc.LocalDescription())
	if err != nil {
		_ = pc.Close()
		return nil, nil, nil, err
	}
	if err := pc.SetRemoteDescription(*answer); err != nil {
		_ = pc.Close()
		return nil, nil, nil, err
	}

	// Wait for ICE to complete, with a cap so a dead SFU doesn't stall
	// the reconnect loop forever.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		switch pc.ConnectionState() {
		case webrtc.PeerConnectionStateConnected:
			return pc, sender, transceiver, nil
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			_ = pc.Close()
			return nil, nil, nil, fmt.Errorf("PC reached terminal state during connect")
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = pc.Close()
	return nil, nil, nil, fmt.Errorf("timeout waiting for PC to connect")
}

func discoverExtensions(sender *webrtc.RTPSender, tr *webrtc.RTPTransceiver) (*extIDs, error) {
	params := sender.GetParameters()
	var e extIDs
	for _, ext := range params.HeaderExtensions {
		switch ext.URI {
		case "urn:ietf:params:rtp-hdrext:sdes:mid":
			e.midID = uint8(ext.ID)
		case "urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id":
			e.ridID = uint8(ext.ID)
		}
	}
	if e.midID == 0 || e.ridID == 0 {
		return nil, fmt.Errorf("MID or RID extension not negotiated (mid=%d rid=%d)",
			e.midID, e.ridID)
	}
	e.mid = tr.Mid()
	return &e, nil
}

// listenAndForward receives RTP packets from GStreamer and forwards them
// into the shared track. Timestamps and sequence numbers are PRESERVED as
// GStreamer set them; only MID and RID header extensions are added so the
// SFU can demux layers. When there is no active PC, packets are dropped.
func listenAndForward(
	port int,
	rid string,
	track *webrtc.TrackLocalStaticRTP,
	ext *atomic.Pointer[extIDs],
) {
	addr := fmt.Sprintf(":%d", port)
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v", addr, err)
	}
	defer conn.Close()
	log.Printf("[%s] listening on %s", rid, addr)

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			log.Printf("[%s] read error: %v", rid, err)
			continue
		}

		e := ext.Load()
		if e == nil {
			// No active PC; drop. Normal during a reconnect window.
			continue
		}

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			log.Printf("[%s] invalid RTP packet: %v", rid, err)
			continue
		}

		_ = pkt.Header.SetExtension(e.midID, []byte(e.mid))
		_ = pkt.Header.SetExtension(e.ridID, []byte(rid))

		if err := track.WriteRTP(pkt); err != nil {
			// Track has no active sender right now; drop.
			continue
		}
	}
}

// waitForClose blocks until the PC enters a terminal state. It checks
// the current state first, so a PC that already failed doesn't hang.
func waitForClose(pc *webrtc.PeerConnection) {
	done := make(chan struct{}, 1)
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("bridge: pc state: %s", s.String())
		if s == webrtc.PeerConnectionStateFailed ||
			s == webrtc.PeerConnectionStateClosed ||
			s == webrtc.PeerConnectionStateDisconnected {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	})
	switch pc.ConnectionState() {
	case webrtc.PeerConnectionStateFailed,
		webrtc.PeerConnectionStateClosed,
		webrtc.PeerConnectionStateDisconnected:
		return
	}
	<-done
}

func postSDP(url string, local *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	body, err := json.Marshal(local)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, raw)
	}
	var answer webrtc.SessionDescription
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, err
	}
	return &answer, nil
}
