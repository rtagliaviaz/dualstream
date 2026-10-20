package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/pion/webrtc/v4"
)

const sfuURL = "http://localhost:8080"

func main() {
	rid := flag.String("rid", "h", "layer to subscribe to: h, m, or l")
	flag.Parse()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}
	defer pc.Close()

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("[viewer rid=%s] received track: kind=%s codec=%s ssrc=%d",
			*rid, track.Kind(), track.Codec().MimeType, track.SSRC())

		var count int
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				log.Printf("[viewer rid=%s] read error: %v", *rid, err)
				return
			}
			count++
			if count%30 == 1 {
				log.Printf("[viewer rid=%s] rtp seq=%d size=%d payload=%q",
					*rid, pkt.SequenceNumber, len(pkt.Payload), string(pkt.Payload))
			}
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[viewer rid=%s] state: %s", *rid, state.String())
	})

	if _, err := pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		},
	); err != nil {
		panic(err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		panic(err)
	}
	<-gatherDone

	url := fmt.Sprintf("%s/subscribe?rid=%s", sfuURL, *rid)
	log.Printf("subscribing to layer %q", *rid)
	answer, err := postSDP(url, pc.LocalDescription())
	if err != nil {
		log.Fatalf("subscribe failed: %v", err)
	}

	if err := pc.SetRemoteDescription(*answer); err != nil {
		panic(err)
	}
	log.Println("subscribed. waiting for video...")

	select {}
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
