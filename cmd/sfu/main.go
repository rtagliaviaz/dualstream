package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/rtagliaviaz/dualstream/internal/hls"
)

var layerOrder = []string{"h", "m", "l"}

const switchCooldown = 10 * time.Second
const upscaleFactor = 1.5
const downscaleFactor = 0.8
const timestampStep uint32 = 3000

// Lifecycle tunables.
const (
	layerInactivityTimeout = 5 * time.Second
	lifecycleTickInterval  = 2 * time.Second
)

type SFUServer struct {
	mu           sync.RWMutex
	broadcaster  *webrtc.PeerConnection
	layers       map[string]*layerState
	debugBitrate uint64
	hls          *hls.Server
}

type layerState struct {
	source  *webrtc.TrackRemote
	mu      sync.RWMutex
	viewers []*viewerConn

	measuredBitrate uint64
	lastPacket      atomic.Int64  // unix nano of last packet
	done            chan struct{} // closed when layer is killed or replaced
}

type viewerConn struct {
	mu           sync.RWMutex
	pc           *webrtc.PeerConnection
	sender       *webrtc.RTPSender
	outTrack     *webrtc.TrackLocalStaticRTP
	currentRID   string
	currentLayer *layerState
	lastSwitch   time.Time
	lastREMB     uint64

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
	hlsDir := os.Getenv("HLS_DIR")
	if hlsDir == "" {
		hlsDir = "./hls"
	}

	sfu := &SFUServer{
		layers: make(map[string]*layerState),
		hls:    hls.New(hlsDir),
	}

	http.Handle("/", http.FileServer(http.Dir("cmd/sfu/static")))
	http.HandleFunc("/broadcast", sfu.handleBroadcast)
	http.HandleFunc("/subscribe", sfu.handleSubscribe)
	http.HandleFunc("/layers", sfu.handleLayers)
	http.HandleFunc("/debug/bitrate", sfu.handleDebugBitrate)
	http.HandleFunc("/hls/info", sfu.handleHLSInfo)
	http.HandleFunc("/reset", sfu.handleReset)
	http.Handle("/hls/", sfu.hls)

	go sfu.lifecycleMonitor()

	log.Println("SFU listening on :8080")
	log.Printf("  HLS_DIR                   → %s", hlsDir)
	log.Println("  GET  /                    → HTML viewer")
	log.Println("  POST /broadcast           → broadcaster publishes 3 layers")
	log.Println("  POST /subscribe?rid=h|m|l → viewer receives one layer")
	log.Println("  GET  /layers              → list layers with measured bitrate")
	log.Println("  POST /debug/bitrate?bps=N → force bitrate (0 = clear)")
	log.Println("  GET  /hls/info            → HLS availability snapshot (JSON)")
	log.Println("  GET  /hls/master.m3u8     → HLS master playlist (fallback)")
	log.Println("  POST /reset               → close broadcaster and all viewers")
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

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		rid := track.RID()
		log.Printf("[layer %s] arrived SSRC=%d", rid, track.SSRC())

		newLs := &layerState{
			source: track,
			done:   make(chan struct{}),
		}
		newLs.lastPacket.Store(time.Now().UnixNano())

		s.mu.Lock()
		oldLs := s.layers[rid]
		s.layers[rid] = newLs
		s.mu.Unlock()

		// Migrate viewers from the old layer, if any. This is the
		// broadcaster-replacement case: same RID, new source track.
		var migrated []*viewerConn
		if oldLs != nil {
			oldLs.mu.Lock()
			migrated = oldLs.viewers
			oldLs.viewers = nil
			oldLs.mu.Unlock()
			close(oldLs.done)
			if len(migrated) > 0 {
				log.Printf("[layer %s] replacing source, migrating %d viewers", rid, len(migrated))
			}
		}

		for _, v := range migrated {
			v.mu.Lock()
			v.currentLayer = newLs
			v.dropUntilFrameEnd = true
			v.lastSwitch = time.Now()
			v.mu.Unlock()
		}

		newLs.mu.Lock()
		newLs.viewers = append(newLs.viewers, migrated...)
		newLs.mu.Unlock()

		var bytesThisSecond atomic.Uint64

		// Bitrate ticker. Stops when the layer is killed or replaced.
		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-newLs.done:
					return
				case <-ticker.C:
					bitrate := bytesThisSecond.Swap(0) * 8
					newLs.mu.Lock()
					newLs.measuredBitrate = bitrate
					newLs.mu.Unlock()
				}
			}
		}()

		// Read loop.
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
				newLs.lastPacket.Store(time.Now().UnixNano())

				newLs.mu.RLock()
				viewers := append([]*viewerConn{}, newLs.viewers...)
				newLs.mu.RUnlock()

				for _, v := range viewers {
					if err := pkt.Unmarshal(buf[:n]); err != nil {
						continue
					}

					v.mu.Lock()
					// currentLayer, not currentRID: distinguishes old
					// and new layerStates with the same RID after a
					// broadcaster replacement.
					if v.currentLayer != newLs {
						v.mu.Unlock()
						continue
					}

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

		// Failed or Closed are terminal; Disconnected is transient and
		// is left to the inactivity monitor.
		if state != webrtc.PeerConnectionStateFailed &&
			state != webrtc.PeerConnectionStateClosed {
			return
		}

		s.mu.Lock()
		if s.broadcaster != pc {
			s.mu.Unlock()
			return
		}
		s.broadcaster = nil
		rids := make([]string, 0, len(s.layers))
		for rid := range s.layers {
			rids = append(rids, rid)
		}
		s.mu.Unlock()

		log.Printf("[broadcaster] connection ended, killing %d layer(s)", len(rids))
		for _, rid := range rids {
			s.killLayer(rid)
		}
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

	// Install the new broadcaster, closing any previous one. The new
	// OnTrack handlers above migrate viewers from the previous layers.
	s.mu.Lock()
	oldBc := s.broadcaster
	s.broadcaster = pc
	s.mu.Unlock()
	if oldBc != nil && oldBc != pc {
		log.Printf("[broadcaster] closing previous peer connection")
		_ = oldBc.Close()
	}

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

	// Wait up to 10 s for the layer to appear.
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

	// The codec is VP8 across all layers and across broadcaster
	// replacements, so reading from the (possibly old) ls.source is safe.
	ls.mu.RLock()
	srcCodec := ls.source.Codec().RTPCodecCapability
	ls.mu.RUnlock()

	outTrack, err := webrtc.NewTrackLocalStaticRTP(srcCodec, "video", "sfu")
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

	// Re-read the layer under the lock and register only if it's still
	// alive. The layer might have been killed or replaced during setup.
	s.mu.RLock()
	currentLs := s.layers[rid]
	s.mu.RUnlock()
	if currentLs == nil {
		_ = pc.Close()
		http.Error(w, fmt.Sprintf("layer %q no longer available", rid),
			http.StatusServiceUnavailable)
		return
	}

	currentLs.mu.Lock()
	select {
	case <-currentLs.done:
		currentLs.mu.Unlock()
		_ = pc.Close()
		http.Error(w, fmt.Sprintf("layer %q died during subscribe", rid),
			http.StatusServiceUnavailable)
		return
	default:
	}
	viewer.currentLayer = currentLs
	currentLs.viewers = append(currentLs.viewers, viewer)
	total := len(currentLs.viewers)
	currentLs.mu.Unlock()

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

	ls.mu.RLock()
	src := ls.source
	ls.mu.RUnlock()
	if src == nil {
		return fmt.Errorf("layer %q has no source", rid)
	}

	ssrc := uint32(src.SSRC())
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
	v.currentLayer = newLayer
	v.lastSwitch = time.Now()
	v.dropUntilFrameEnd = true
	v.mu.Unlock()

	newLayer.mu.Lock()
	select {
	case <-newLayer.done:
		newLayer.mu.Unlock()
		return fmt.Errorf("layer %q died during switch", newRID)
	default:
	}
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

// lifecycleMonitor ticks periodically and kills any layer that has not
// received a packet within layerInactivityTimeout.
func (s *SFUServer) lifecycleMonitor() {
	ticker := time.NewTicker(lifecycleTickInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.checkInactivity()
	}
}

func (s *SFUServer) checkInactivity() {
	now := time.Now().UnixNano()
	timeout := int64(layerInactivityTimeout)

	s.mu.RLock()
	var dead []string
	for rid, ls := range s.layers {
		last := ls.lastPacket.Load()
		if last == 0 {
			continue
		}
		if now-last > timeout {
			dead = append(dead, rid)
		}
	}
	s.mu.RUnlock()

	for _, rid := range dead {
		s.killLayer(rid)
	}
}

// killLayer removes a layer from the map, closes all its viewers' peer
// connections, and signals its ticker to exit. If this was the last layer,
// it also closes the broadcaster connection.
func (s *SFUServer) killLayer(rid string) {
	s.mu.Lock()
	ls, ok := s.layers[rid]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.layers, rid)
	noLayersLeft := len(s.layers) == 0
	bc := s.broadcaster
	s.mu.Unlock()

	ls.mu.Lock()
	viewers := ls.viewers
	ls.viewers = nil
	close(ls.done)
	ls.mu.Unlock()

	for _, v := range viewers {
		v.mu.Lock()
		_ = v.pc.Close()
		v.mu.Unlock()
	}

	log.Printf("[lifecycle] layer %q killed (%d viewers closed)", rid, len(viewers))

	if noLayersLeft && bc != nil {
		log.Printf("[lifecycle] no layers left, closing broadcaster connection")
		_ = bc.Close()
	}
}

func (s *SFUServer) handleLayers(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().UnixNano()
	fmt.Fprintln(w, "rid  measured_bps  viewers  last_pkt")
	for _, rid := range layerOrder {
		ls, ok := s.layers[rid]
		if !ok {
			continue
		}
		ls.mu.RLock()
		measured := ls.measuredBitrate
		viewers := len(ls.viewers)
		ls.mu.RUnlock()
		last := ls.lastPacket.Load()
		var age time.Duration
		if last == 0 {
			age = -1
		} else {
			age = time.Duration(now - last)
		}
		fmt.Fprintf(w, "%-4s %-13d %-8d %s\n", rid, measured, viewers, age)
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

// handleHLSInfo returns a JSON snapshot of the HLS fallback state so the
// browser can decide whether to try HLS before creating a peer connection.
func (s *SFUServer) handleHLSInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(s.hls.Snapshot())
}

// handleReset closes the broadcaster connection, all viewer connections,
// and clears the layer map. Useful for development so you can bring the
// system back to a clean state without restarting the SFU process.
func (s *SFUServer) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	bc := s.broadcaster
	s.broadcaster = nil
	layers := s.layers
	s.layers = make(map[string]*layerState)
	s.mu.Unlock()

	if bc != nil {
		_ = bc.Close()
	}

	totalViewers := 0
	for rid, ls := range layers {
		ls.mu.Lock()
		viewers := ls.viewers
		ls.viewers = nil
		close(ls.done)
		ls.mu.Unlock()

		for _, v := range viewers {
			v.mu.Lock()
			_ = v.pc.Close()
			v.mu.Unlock()
		}
		totalViewers += len(viewers)
		log.Printf("[lifecycle] reset: layer %q closed (%d viewers)", rid, len(viewers))
	}

	log.Printf("[lifecycle] reset complete: broadcaster=%v layers=%d viewers=%d",
		bc != nil, len(layers), totalViewers)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":                 true,
		"broadcasterWasOpen": bc != nil,
		"layersClosed":       len(layers),
		"viewersClosed":      totalViewers,
	})
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
