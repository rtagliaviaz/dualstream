package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const sfuURL = "http://localhost:8080"

func main() {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}
	if err := webrtc.ConfigureSimulcastExtensionHeaders(m); err != nil {
		panic(err)
	}

	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		panic(err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(i),
	)

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}
	defer pc.Close()

	capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}

	trackH, err := webrtc.NewTrackLocalStaticRTP(capability, "video", "broadcaster",
		webrtc.WithRTPStreamID("h"))
	if err != nil {
		panic(err)
	}
	trackM, err := webrtc.NewTrackLocalStaticRTP(capability, "video", "broadcaster",
		webrtc.WithRTPStreamID("m"))
	if err != nil {
		panic(err)
	}
	trackL, err := webrtc.NewTrackLocalStaticRTP(capability, "video", "broadcaster",
		webrtc.WithRTPStreamID("l"))
	if err != nil {
		panic(err)
	}

	transceiver, err := pc.AddTransceiverFromTrack(trackH,
		webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendonly,
			SendEncodings: []webrtc.RTPEncodingParameters{
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: "h"}},
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: "m"}},
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: "l"}},
			},
		},
	)
	if err != nil {
		panic(err)
	}

	sender := transceiver.Sender()

	if err := sender.AddEncoding(trackM); err != nil {
		panic(err)
	}
	if err := sender.AddEncoding(trackL); err != nil {
		panic(err)
	}

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
		panic(err)
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		panic(err)
	}
	<-gatherDone

	log.Println("sending offer to SFU...")
	answer, err := postSDP(sfuURL+"/broadcast", pc.LocalDescription())
	if err != nil {
		log.Fatalf("broadcast failed: %v", err)
	}

	if err := pc.SetRemoteDescription(*answer); err != nil {
		panic(err)
	}
	log.Println("connected to SFU as broadcaster")

	waitForConnected(pc)

	params := sender.GetParameters()
	var midID, ridID uint8
	for _, ext := range params.HeaderExtensions {
		switch ext.URI {
		case "urn:ietf:params:rtp-hdrext:sdes:mid":
			midID = uint8(ext.ID)
		case "urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id":
			ridID = uint8(ext.ID)
		}
	}
	if midID == 0 || ridID == 0 {
		log.Fatalf("MID or RID extension not negotiated (mid=%d rid=%d)", midID, ridID)
	}
	mid := transceiver.Mid()
	log.Printf("negotiated extension IDs: MID=%d RID=%d (mid=%q)", midID, ridID, mid)

	go listenAndForward(5000, "h", trackH, midID, ridID, mid)
	go listenAndForward(5001, "m", trackM, midID, ridID, mid)
	go listenAndForward(5002, "l", trackL, midID, ridID, mid)

	log.Println("bridge listening on UDP :5000, :5001, :5002")
	select {}
}

// listenAndForward receives RTP packets from GStreamer and forwards them
// into the WebRTC track. Timestamps and sequence numbers are PRESERVED as
// GStreamer set them; only the MID and RID header extensions are added so
// the SFU can demux layers.
func listenAndForward(
	port int,
	rid string,
	track *webrtc.TrackLocalStaticRTP,
	midID, ridID uint8,
	mid string,
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

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			log.Printf("[%s] invalid RTP packet: %v", rid, err)
			continue
		}

		_ = pkt.Header.SetExtension(midID, []byte(mid))
		_ = pkt.Header.SetExtension(ridID, []byte(rid))

		if err := track.WriteRTP(pkt); err != nil {
			log.Printf("[%s] write error: %v", rid, err)
		}
	}
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

func waitForConnected(pc *webrtc.PeerConnection) {
	for {
		s := pc.ConnectionState()
		if s == webrtc.PeerConnectionStateConnected {
			return
		}
		if s == webrtc.PeerConnectionStateFailed ||
			s == webrtc.PeerConnectionStateClosed {
			panic("connection failed")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
