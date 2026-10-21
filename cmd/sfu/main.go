package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var layerBitrates = map[string]uint64{
	"h": 2_000_000,
	"m": 1_000_000,
	"l": 500_000,
}

var layerOrder = []string{"h", "m", "l"}

const switchCooldown = 10 * time.Second
const upscaleFactor = 1.5
const downscaleFactor = 0.8
const timestampStep uint32 = 3000

type SFUServer struct {
	mu           sync.RWMutex
	broadcaster  *webrtc.PeerConnection
	layers       map[string]*layerState
	debugBitrate uint64
}

type layerState struct {
	source  *webrtc.TrackRemote
	mu      sync.RWMutex
	viewers []*viewerConn

	measuredBitrate uint64
}

type viewerConn struct {
	mu         sync.RWMutex
	pc         *webrtc.PeerConnection
	sender     *webrtc.RTPSender
	outTrack   *webrtc.TrackLocalStaticRTP
	currentRID string
	lastSwitch time.Time
	lastREMB   uint64

	// Stable stream identity across layer switches.
	stableSSRC uint32
	nextSeq    uint16
	frameTime  uint32
	lastMarker bool

	// When true, packets are dropped until the next frame boundary
	// (marker=true). This prevents mixing macroblocks from a mid-frame
	// switch, which shows as a purple stain or a freeze.
	dropUntilFrameEnd bool
}

func main() {
	sfu := &SFUServer{layers: make(map[string]*layerState)}

	http.Handle("/", http.FileServer(http.Dir("cmd/sfu/static")))
	http.HandleFunc("/broadcast", sfu.handleBroadcast)
	http.HandleFunc("/subscribe", sfu.handleSubscribe)
	http.HandleFunc("/layers", sfu.handleLayers)
	http.HandleFunc("/debug/bitrate", sfu.handleDebugBitrate)

	log.Println("SFU listening on :8080")
	log.Println("  GET  /                    → HTML viewer")
	log.Println("  POST /broadcast           → broadcaster publishes 3 layers")
	log.Println("  POST /subscribe?rid=h|m|l → viewer receives one layer")
	log.Println("  GET  /layers              → list layers with measured bitrate")
	log.Println("  POST /debug/bitrate?bps=N → force bitrate (0 = clear)")
	log.Fatal(http.ListenAndServe("0.0.0.0:8080", nil))
}

func (s *SFUServer) handleBroadcast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	pc, err := newPeerConnection()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		rid := track.RID()
		log.Printf("[layer %s] arrived SSRC=%d", rid, track.SSRC())

		ls := &layerState{source: track}

		s.mu.Lock()
		s.layers[rid] = ls
		s.mu.Unlock()

		var bytesThisSecond atomic.Uint64
		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				bitrate := bytesThisSecond.Swap(0) * 8
				ls.mu.Lock()
				ls.measuredBitrate = bitrate
				ls.mu.Unlock()
			}
		}()

		go func() {
			buf := make([]byte, 1500)
			pkt := &rtp.Packet{}
			for {
				n, _, err := track.Read(buf)
				if err != nil {
					log.Printf("[layer %s] read error: %v", rid, err)
					return
				}
				bytesThisSecond.Add(uint64(n))

				ls.mu.RLock()
				viewers := append([]*viewerConn{}, ls.viewers...)
				ls.mu.RUnlock()

				for _, v := range viewers {
					if err := pkt.Unmarshal(buf[:n]); err != nil {
						continue
					}

					v.mu.Lock()
					if v.currentRID != rid {
						v.mu.Unlock()
						continue
					}

					// Just switched: drop until we see the end of the
					// current frame. The next packet after marker=true
					// will be a clean frame start (a keyframe, since
					// keyframe-max-dist=1).
					if v.dropUntilFrameEnd {
						v.lastMarker = pkt.Header.Marker
						if pkt.Header.Marker {
							v.dropUntilFrameEnd = false
						}
						v.mu.Unlock()
						continue
					}

					pkt.Header.SSRC = v.stableSSRC
					pkt.Header.SequenceNumber = v.nextSeq
					v.nextSeq++

					if v.lastMarker {
						v.frameTime += timestampStep
					}
					pkt.Header.Timestamp = v.frameTime
					v.lastMarker = pkt.Header.Marker

					outTrack := v.outTrack
					v.mu.Unlock()

					if err := outTrack.WriteRTP(pkt); err != nil {
						log.Printf("[layer %s] write error: %v", rid, err)
					}
				}
			}
		}()
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[broadcaster] state: %s", state.String())
	})

	if _, err := pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-gatherDone

	s.mu.Lock()
	s.broadcaster = pc
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pc.LocalDescription())
}

func (s *SFUServer) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	rid := r.URL.Query().Get("rid")
	if rid == "" {
		rid = "h"
	}

	var ls *layerState
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		ls = s.layers[rid]
		s.mu.RUnlock()
		if ls != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if ls == nil {
		http.Error(w, fmt.Sprintf("layer %q not available after 10s", rid),
			http.StatusServiceUnavailable)
		return
	}

	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	pc, err := newPeerConnection()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	outTrack, err := webrtc.NewTrackLocalStaticRTP(
		ls.source.Codec().RTPCodecCapability,
		"video",
		"sfu",
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rtpSender, err := pc.AddTrack(outTrack)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	viewer := &viewerConn{
		pc:         pc,
		sender:     rtpSender,
		outTrack:   outTrack,
		currentRID: rid,
		lastSwitch: time.Now(),
		stableSSRC: uint32(time.Now().UnixNano()),
		nextSeq:    uint16(time.Now().UnixNano() & 0xFFFF),
	}

	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := rtpSender.Read(buf)
			if err != nil {
				return
			}
			pkts, err := rtcp.Unmarshal(buf[:n])
			if err != nil {
				continue
			}
			for _, pkt := range pkts {
				if remb, ok := pkt.(*rtcp.ReceiverEstimatedMaximumBitrate); ok {
					s.onREMB(viewer, uint64(remb.Bitrate))
				}
			}
		}
	}()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[viewer] state: %s", state.String())
		if state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateFailed {
			s.removeViewerFromAllLayers(viewer)
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-gatherDone

	ls.mu.Lock()
	ls.viewers = append(ls.viewers, viewer)
	total := len(ls.viewers)
	ls.mu.Unlock()

	log.Printf("[viewer] registered on %q (layer total: %d)", rid, total)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pc.LocalDescription())
}

func (s *SFUServer) onREMB(v *viewerConn, bitrate uint64) {
	s.mu.RLock()
	debug := s.debugBitrate
	s.mu.RUnlock()
	if debug > 0 {
		bitrate = debug
	}

	v.mu.Lock()
	v.lastREMB = bitrate
	currentRID := v.currentRID
	lastSwitch := v.lastSwitch
	v.mu.Unlock()

	if time.Since(lastSwitch) < switchCooldown {
		return
	}

	targetRID := s.selectLayer(bitrate, currentRID)
	if targetRID == currentRID {
		return
	}

	log.Printf("[switch] %q → %q  (REMB=%d bps)", currentRID, targetRID, bitrate)

	if err := s.requestKeyframe(targetRID); err != nil {
		log.Printf("[switch] requestKeyframe(%s): %v", targetRID, err)
	}

	if err := s.switchLayer(v, targetRID); err != nil {
		log.Printf("[switch] failed: %v", err)
		return
	}
}

func (s *SFUServer) requestKeyframe(rid string) error {
	s.mu.RLock()
	bc := s.broadcaster
	ls := s.layers[rid]
	s.mu.RUnlock()

	if bc == nil {
		return fmt.Errorf("no broadcaster connected")
	}
	if ls == nil {
		return fmt.Errorf("layer %q not available", rid)
	}

	ssrc := uint32(ls.source.SSRC())
	return bc.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: ssrc},
	})
}

func (s *SFUServer) selectLayer(bitrate uint64, currentRID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	measured := func(rid string) uint64 {
		if ls, ok := s.layers[rid]; ok {
			ls.mu.RLock()
			b := ls.measuredBitrate
			ls.mu.RUnlock()
			return b
		}
		return 0
	}

	currentTarget := measured(currentRID)

	if currentTarget > 0 && float64(bitrate) < float64(currentTarget)*downscaleFactor {
		currentIdx := -1
		for i, rid := range layerOrder {
			if rid == currentRID {
				currentIdx = i
				break
			}
		}
		if currentIdx >= 0 {
			for i := currentIdx + 1; i < len(layerOrder); i++ {
				rid := layerOrder[i]
				target := measured(rid)
				if target == 0 {
					continue
				}
				if float64(bitrate) >= float64(target)*downscaleFactor {
					return rid
				}
			}
		}
		for i := len(layerOrder) - 1; i >= 0; i-- {
			if measured(layerOrder[i]) > 0 {
				return layerOrder[i]
			}
		}
		return currentRID
	}

	for _, rid := range layerOrder {
		if rid == currentRID {
			return currentRID
		}
		target := measured(rid)
		if target == 0 {
			continue
		}
		if float64(bitrate) >= float64(target)*upscaleFactor {
			return rid
		}
	}

	return currentRID
}

// switchLayer updates which fan-out writes to this viewer. The outTrack,
// SSRC, sequence number and timestamp continuity are preserved so the
// browser sees a single unbroken stream. Packets are dropped until the
// next frame boundary so the decoder never sees a mid-frame resolution
// change.
func (s *SFUServer) switchLayer(v *viewerConn, newRID string) error {
	s.mu.RLock()
	newLayer := s.layers[newRID]
	s.mu.RUnlock()
	if newLayer == nil {
		return fmt.Errorf("layer %q not available", newRID)
	}

	s.removeViewerFromAllLayers(v)

	v.mu.Lock()
	v.currentRID = newRID
	v.lastSwitch = time.Now()
	v.dropUntilFrameEnd = true
	v.mu.Unlock()

	newLayer.mu.Lock()
	newLayer.viewers = append(newLayer.viewers, v)
	newLayer.mu.Unlock()

	return nil
}

func (s *SFUServer) removeViewerFromAllLayers(v *viewerConn) {
	s.mu.RLock()
	allLayers := make([]*layerState, 0, len(s.layers))
	for _, ls := range s.layers {
		allLayers = append(allLayers, ls)
	}
	s.mu.RUnlock()

	for _, ls := range allLayers {
		ls.mu.Lock()
		for i, existing := range ls.viewers {
			if existing == v {
				ls.viewers = append(ls.viewers[:i], ls.viewers[i+1:]...)
				break
			}
		}
		ls.mu.Unlock()
	}
}

func (s *SFUServer) handleLayers(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fmt.Fprintln(w, "rid  measured_bps  viewers")
	for _, rid := range layerOrder {
		ls, ok := s.layers[rid]
		if !ok {
			continue
		}
		ls.mu.RLock()
		measured := ls.measuredBitrate
		viewers := len(ls.viewers)
		ls.mu.RUnlock()
		fmt.Fprintf(w, "%-4s %-13d %d\n", rid, measured, viewers)
	}
}

func (s *SFUServer) handleDebugBitrate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	bps, err := strconv.ParseUint(r.URL.Query().Get("bps"), 10, 64)
	if err != nil {
		http.Error(w, "invalid bps", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.debugBitrate = bps
	s.mu.Unlock()
	if bps > 0 {
		fmt.Fprintf(w, "forced bitrate: %d bps\n", bps)
	} else {
		fmt.Fprintln(w, "bitrate override cleared")
	}
}

func newPeerConnection() (*webrtc.PeerConnection, error) {
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

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(i),
	)
	return api.NewPeerConnection(webrtc.Configuration{})
}
