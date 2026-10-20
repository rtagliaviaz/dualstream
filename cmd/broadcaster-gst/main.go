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
	// 1. Configurar PeerConnection con las extensiones de simulcast.
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
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}
	defer pc.Close()

	// 2. Crear los tres tracks (h, m, l) para el simulcast.
	capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}
	trackH, _ := webrtc.NewTrackLocalStaticRTP(capability, "video", "broadcaster", webrtc.WithRTPStreamID("h"))
	trackM, _ := webrtc.NewTrackLocalStaticRTP(capability, "video", "broadcaster", webrtc.WithRTPStreamID("m"))
	trackL, _ := webrtc.NewTrackLocalStaticRTP(capability, "video", "broadcaster", webrtc.WithRTPStreamID("l"))

	// 3. Añadir el track de la capa alta y configurar el simulcast.
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

	// Drenar RTCP.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()

	// 4. Signalización con el SFU (HTTP).
	offer, _ := pc.CreateOffer(nil)
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		panic(err)
	}
	<-gatherDone

	answer, err := postSDP(sfuURL+"/broadcast", pc.LocalDescription())
	if err != nil {
		log.Fatalf("broadcast failed: %v", err)
	}
	if err := pc.SetRemoteDescription(*answer); err != nil {
		panic(err)
	}
	waitForConnected(pc)

	// 5. Obtener los IDs negociados para las extensiones MID y RID.
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
		log.Fatalf("extensiones MID o RID no negociadas (mid=%d, rid=%d)", midID, ridID)
	}
	mid := transceiver.Mid()
	log.Printf("Extensiones negociadas: MID=%d, RID=%d (mid=%q)", midID, ridID, mid)

	// 6. Escuchar en los tres puertos UDP y reenviar a los tracks.
	go listenAndForward(5000, "h", trackH, midID, ridID, mid)
	go listenAndForward(5001, "m", trackM, midID, ridID, mid)
	go listenAndForward(5002, "l", trackL, midID, ridID, mid)

	log.Println("Puente GStreamer → WebRTC escuchando en UDP :5000, :5001, :5002")
	select {}
}

// listenAndForward recibe paquetes RTP de GStreamer y los escribe en un track.
func listenAndForward(port int, rid string, track *webrtc.TrackLocalStaticRTP, midID, ridID uint8, mid string) {
	addr := fmt.Sprintf(":%d", port)
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatalf("no se pudo escuchar en %s: %v", addr, err)
	}
	defer conn.Close()
	log.Printf("[%s] escuchando en %s", rid, addr)

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			log.Printf("[%s] error de lectura: %v", rid, err)
			continue
		}

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			log.Printf("[%s] paquete RTP inválido: %v", rid, err)
			continue
		}

		// Añadir las extensiones MID y RID.
		_ = pkt.Header.SetExtension(midID, []byte(mid))
		_ = pkt.Header.SetExtension(ridID, []byte(rid))

		// Escribir en el track de WebRTC.
		if err := track.WriteRTP(pkt); err != nil {
			log.Printf("[%s] error al escribir en el track: %v", rid, err)
		}
	}
}

// --- Funciones auxiliares (postSDP, waitForConnected) ---
func postSDP(url string, local *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	body, _ := json.Marshal(local)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("servidor devolvió %d: %s", resp.StatusCode, raw)
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
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			panic("falló la conexión")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// gst-launch-1.0 -v `
//   videotestsrc is-live=true ! `
//   video/x-raw,width=1280,height=720,format=I420,framerate=30/1 ! `
//   tee name=t `
//   t. ! queue ! videoscale ! video/x-raw,width=1280,height=720 ! vp8enc deadline=1 keyframe-max-dist=30 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5000 `
//   t. ! queue ! videoscale ! video/x-raw,width=854,height=480  ! vp8enc deadline=1 keyframe-max-dist=30 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5001 `
//   t. ! queue ! videoscale ! video/x-raw,width=640,height=360  ! vp8enc deadline=1 keyframe-max-dist=30 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5002

// gst-launch-1.0 -v `
//   filesrc location="C:\Users\rtagl\OneDrive\Escritorio\sample-1.mkv" ! `
//   decodebin ! `
//   videoconvert ! `
//   video/x-raw,format=I420 ! `
//   tee name=t `
//   t. ! queue ! videoscale ! video/x-raw,width=1280,height=720 ! vp8enc deadline=1 keyframe-max-dist=30 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5000 `
//   t. ! queue ! videoscale ! video/x-raw,width=854,height=480 ! vp8enc deadline=1 keyframe-max-dist=30 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5001 `
//   t. ! queue ! videoscale ! video/x-raw,width=640,height=360 ! vp8enc deadline=1 keyframe-max-dist=30 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5002
