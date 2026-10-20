package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

type SFUServer struct {
	mu          sync.RWMutex
	broadcaster *webrtc.PeerConnection
	layers      map[string]*layerState // key = RID ("h", "m", "l")
}

type layerState struct {
	source  *webrtc.TrackRemote
	mu      sync.RWMutex
	viewers []*viewerConn
}

type viewerConn struct {
	pc       *webrtc.PeerConnection
	outTrack *webrtc.TrackLocalStaticRTP
}

func main() {
	sfu := &SFUServer{
		layers: make(map[string]*layerState),
	}

	http.Handle("/", http.FileServer(http.Dir("cmd/sfu/static")))

	http.HandleFunc("/broadcast", sfu.handleBroadcast)
	http.HandleFunc("/subscribe", sfu.handleSubscribe)
	http.HandleFunc("/layers", sfu.handleLayers)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	log.Println("SFU listening on :8080")

	log.Println("  GET  /                    → HTML viewer")

	log.Println("  POST /broadcast          → broadcaster publishes 3 layers")
	log.Println("  POST /subscribe?rid=h|m|l → viewer receives one layer")
	log.Println("  GET  /layers             → list available layers")
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

	// Register the OnTrack handler BEFORE applying the remote description.
	// It fires once per simulcast layer that the broadcaster publishes.
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		rid := track.RID()
		log.Printf("[broadcaster] layer arrived: RID=%q SSRC=%d codec=%s",
			rid, track.SSRC(), track.Codec().MimeType)

		ls := &layerState{source: track}

		s.mu.Lock()
		s.layers[rid] = ls
		s.mu.Unlock()

		// Fan-out loop for this layer.
		go func() {
			buf := make([]byte, 1500)
			for {
				n, _, err := track.Read(buf)
				if err != nil {
					log.Printf("[layer %s] read error: %v", rid, err)
					return
				}

				ls.mu.RLock()
				viewers := append([]*viewerConn{}, ls.viewers...)
				ls.mu.RUnlock()

				for _, v := range viewers {
					if _, err := v.outTrack.Write(buf[:n]); err != nil {
						log.Printf("[layer %s] write to viewer error: %v", rid, err)
					}
				}
			}
		}()

		log.Printf("[layer %s] ready for viewers", rid)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[broadcaster] state: %s", state.String())
	})

	// Add a recvonly video transceiver BEFORE applying the offer. This
	// tells Pion explicitly that we expect to receive video from the
	// broadcaster, which is required to accept simulcast streams.
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

	// Wait up to 10 seconds for the requested layer to become available.
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
		"sfu-"+rid,
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

	// Drain RTCP feedback.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(buf); err != nil {
				return
			}
		}
	}()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[viewer rid=%s] state: %s", rid, state.String())
		if state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateFailed {
			ls.mu.Lock()
			for i, v := range ls.viewers {
				if v.pc == pc {
					ls.viewers = append(ls.viewers[:i], ls.viewers[i+1:]...)
					break
				}
			}
			ls.mu.Unlock()
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
	ls.viewers = append(ls.viewers, &viewerConn{pc: pc, outTrack: outTrack})
	total := len(ls.viewers)
	ls.mu.Unlock()

	log.Printf("[viewer rid=%s] registered (layer total: %d)", rid, total)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pc.LocalDescription())
}

func (s *SFUServer) handleLayers(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for rid, ls := range s.layers {
		fmt.Fprintf(w, "rid=%s ssrc=%d viewers=%d\n",
			rid, ls.source.SSRC(), len(ls.viewers))
	}
}

// newPeerConnection creates a PeerConnection with the simulcast header
// extensions (MID, RID, RSID) enabled. All three are required for Pion
// to demux simulcast layers and populate track.RID().
func newPeerConnection() (*webrtc.PeerConnection, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureSimulcastExtensionHeaders(m); err != nil {
		return nil, err
	}

	// Register the default interceptors explicitly. Without them, the SFU
	// cannot demux incoming simulcast layers even if the SDP advertises them.
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
