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

// extIDs holds the negotiated extension IDs and the MIDs for both
// transceivers of the current PC. Stored atomically and read by the
// forever-running UDP forwarders.
type extIDs struct {
	midID, ridID uint8
	videoMid     string
	audioMid     string
}

type udpLayer struct {
	port  int
	rid   string // "h"/"m"/"l" for video, "" for audio
	track *webrtc.TrackLocalStaticRTP
}

func main() {
	api, err := newAPI()
	if err != nil {
		log.Fatalf("webrtc api: %v", err)
	}

	videoCodec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}
	audioCodec := webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	}

	mustVideoTrack := func(rid string) *webrtc.TrackLocalStaticRTP {
		t, err := webrtc.NewTrackLocalStaticRTP(videoCodec, "video", "broadcaster",
			webrtc.WithRTPStreamID(rid))
		if err != nil {
			log.Fatalf("track %s: %v", rid, err)
		}
		return t
	}
	mustAudioTrack := func() *webrtc.TrackLocalStaticRTP {
		t, err := webrtc.NewTrackLocalStaticRTP(audioCodec, "audio", "broadcaster")
		if err != nil {
			log.Fatalf("audio track: %v", err)
		}
		return t
	}

	layers := []udpLayer{
		{port: 5000, rid: "h", track: mustVideoTrack("h")},
		{port: 5001, rid: "m", track: mustVideoTrack("m")},
		{port: 5002, rid: "l", track: mustVideoTrack("l")},
		{port: 5004, rid: "", track: mustAudioTrack()},
	}

	var currentExt atomic.Pointer[extIDs]
	for _, l := range layers {
		l := l
		go listenAndForward(l, &currentExt)
	}

	backoff := initialBackoff
	for {
		pc, senders, err := connectAndPublish(api, layers)
		if err != nil {
			log.Printf("bridge: connect failed: %v (retry in %s)", err, backoff)
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = initialBackoff

		ext, err := discoverExtensions(pc, senders)
		if err != nil {
			log.Printf("bridge: extension discovery failed: %v", err)
			_ = pc.Close()
			continue
		}
		currentExt.Store(ext)
		log.Printf("bridge: connected (MID=%d RID=%d videoMid=%q audioMid=%q)",
			ext.midID, ext.ridID, ext.videoMid, ext.audioMid)

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

// connectAndPublish builds a fresh PC with two transceivers (video
// simulcast + audio), negotiates with the SFU, and waits for ICE.
func connectAndPublish(api *webrtc.API, layers []udpLayer) (
	*webrtc.PeerConnection, []*webrtc.RTPSender, error,
) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, nil, err
	}

	videoTrack := layers[0].track
	audioTrack := layers[3].track

	// Video transceiver with three simulcast encodings.
	videoTr, err := pc.AddTransceiverFromTrack(videoTrack,
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
		return nil, nil, err
	}
	videoSender := videoTr.Sender()
	if err := videoSender.AddEncoding(layers[1].track); err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	if err := videoSender.AddEncoding(layers[2].track); err != nil {
		_ = pc.Close()
		return nil, nil, err
	}

	// Audio transceiver (single track, no simulcast).
	audioTr, err := pc.AddTransceiverFromTrack(audioTrack,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly},
	)
	if err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	audioSender := audioTr.Sender()

	// Drain RTCP from both senders.
	for _, s := range []*webrtc.RTPSender{videoSender, audioSender} {
		go func(s *webrtc.RTPSender) {
			buf := make([]byte, 1500)
			for {
				if _, _, err := s.Read(buf); err != nil {
					return
				}
			}
		}(s)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	<-gatherDone

	log.Println("bridge: sending offer to SFU…")
	answer, err := postSDP(sfuURL+"/broadcast", pc.LocalDescription())
	if err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	if err := pc.SetRemoteDescription(*answer); err != nil {
		_ = pc.Close()
		return nil, nil, err
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		switch pc.ConnectionState() {
		case webrtc.PeerConnectionStateConnected:
			return pc, []*webrtc.RTPSender{videoSender, audioSender}, nil
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			_ = pc.Close()
			return nil, nil, fmt.Errorf("PC reached terminal state during connect")
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = pc.Close()
	return nil, nil, fmt.Errorf("timeout waiting for PC to connect")
}

func discoverExtensions(
	pc *webrtc.PeerConnection,
	senders []*webrtc.RTPSender,
) (*extIDs, error) {
	params := senders[0].GetParameters() // video sender
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
	for _, t := range pc.GetTransceivers() {
		if t.Mid() == "" {
			continue
		}
		sender := t.Sender()
		if sender == nil {
			continue
		}
		track := sender.Track()
		if track == nil {
			continue
		}
		switch track.Kind() {
		case webrtc.RTPCodecTypeVideo:
			e.videoMid = t.Mid()
		case webrtc.RTPCodecTypeAudio:
			e.audioMid = t.Mid()
		}
	}
	if e.videoMid == "" || e.audioMid == "" {
		return nil, fmt.Errorf("MID not resolved (video=%q audio=%q)",
			e.videoMid, e.audioMid)
	}
	return &e, nil
}

// listenAndForward reads RTP from a UDP port and writes it to the track.
// Video packets get MID+RID extensions; audio packets only get MID.
func listenAndForward(l udpLayer, ext *atomic.Pointer[extIDs]) {
	addr := fmt.Sprintf(":%d", l.port)
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v", addr, err)
	}
	defer conn.Close()
	label := l.rid
	if label == "" {
		label = "audio"
	}
	log.Printf("[%s] listening on %s", label, addr)

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			log.Printf("[%s] read error: %v", label, err)
			continue
		}
		e := ext.Load()
		if e == nil {
			continue
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			log.Printf("[%s] invalid RTP packet: %v", label, err)
			continue
		}
		if l.rid == "" {
			_ = pkt.Header.SetExtension(e.midID, []byte(e.audioMid))
		} else {
			_ = pkt.Header.SetExtension(e.midID, []byte(e.videoMid))
			_ = pkt.Header.SetExtension(e.ridID, []byte(l.rid))
		}
		if err := l.track.WriteRTP(pkt); err != nil {
			continue
		}
	}
}

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