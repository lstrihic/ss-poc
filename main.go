package main

import (
	"encoding/json"
	"flag"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"html/template"
	"log"
	"net/http"
	"sync"
	"time"

	// Embed the index.html file
	_ "embed"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// Embed the index.html content
//
//go:embed index.html
var indexHTML string

var (
	addr     = flag.String("addr", ":8080", "HTTP service address")
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	indexTemplate *template.Template

	peerConnectionsMu sync.RWMutex
	peerConnections   []peerConnectionState
	trackLocals       map[string]*webrtc.TrackLocalStaticRTP
)

// websocketMessage represents the structure of messages sent over the WebSocket
type websocketMessage struct {
	Event     string `json:"event"`
	Data      string `json:"data"`
	Role      string `json:"role"`
	UserID    string `json:"userId"`
	TrackType string `json:"trackType"` // "screen" or "camera"
}

// peerConnectionState holds the state of a peer connection
type peerConnectionState struct {
	peerConnection *webrtc.PeerConnection
	websocket      *threadSafeWriter
	role           string
	userID         string
}

func main() {
	flag.Parse()
	trackLocals = make(map[string]*webrtc.TrackLocalStaticRTP)

	var err error
	indexTemplate, err = template.New("").Parse(indexHTML)
	if err != nil {
		log.Fatalf("Failed to parse index template: %v", err)
	}

	http.HandleFunc("/websocket", websocketHandler)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err = indexTemplate.Execute(w, "ws://"+r.Host+"/websocket"); err != nil {
			log.Printf("Failed to execute index template: %v", err)
		}
	})

	// Periodically dispatch key frames
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			dispatchKeyFrame()
		}
	}()

	log.Printf("Server starting at %s", *addr)
	if err = http.ListenAndServe(*addr, nil); err != nil {
		log.Fatalf("Failed to start HTTP server: %v", err)
	}
}

func addTrack(t *webrtc.TrackRemote, userID, trackType string) (*webrtc.TrackLocalStaticRTP, error) {
	peerConnectionsMu.Lock()
	defer func() {
		peerConnectionsMu.Unlock()
		signalPeerConnections()
	}()

	trackID := userID + "-" + trackType + "-" + t.ID()
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, trackID, userID)
	if err != nil {
		return nil, err
	}

	trackLocals[trackID] = trackLocal
	return trackLocal, nil
}

func removeTrack(t *webrtc.TrackLocalStaticRTP) {
	peerConnectionsMu.Lock()
	defer func() {
		peerConnectionsMu.Unlock()
		signalPeerConnections()
	}()

	delete(trackLocals, t.ID())
}

func signalPeerConnections() {
	peerConnectionsMu.Lock()
	defer func() {
		peerConnectionsMu.Unlock()
		dispatchKeyFrame()
	}()

	// Clean up closed peer connections
	var activeConnections []peerConnectionState
	for _, pcState := range peerConnections {
		if pcState.peerConnection.ConnectionState() != webrtc.PeerConnectionStateClosed {
			activeConnections = append(activeConnections, pcState)
		}
	}
	peerConnections = activeConnections

	for _, pcState := range peerConnections {
		pc := pcState.peerConnection

		existingSenders := make(map[string]bool)
		for _, sender := range pc.GetSenders() {
			if sender.Track() != nil {
				existingSenders[sender.Track().ID()] = true
			}
		}

		for trackID, trackLocal := range trackLocals {
			if !existingSenders[trackID] {
				shouldAddTrack := false

				switch pcState.role {
				case "instructor":
					if trackLocal.StreamID() != pcState.userID {
						shouldAddTrack = true
					}
				case "student":
					for _, otherPC := range peerConnections {
						if otherPC.role == "instructor" && trackLocal.StreamID() == otherPC.userID {
							shouldAddTrack = true
							break
						}
					}
				}

				if shouldAddTrack {
					if _, err := pc.AddTrack(trackLocal); err != nil {
						log.Printf("Failed to add track: %v", err)
						continue
					}
				}
			}
		}

		offer, err := pc.CreateOffer(nil)
		if err != nil {
			log.Printf("Failed to create offer: %v", err)
			continue
		}

		if err = pc.SetLocalDescription(offer); err != nil {
			log.Printf("Failed to set local description: %v", err)
			continue
		}

		offerJSON, err := json.Marshal(offer)
		if err != nil {
			log.Printf("Failed to marshal offer: %v", err)
			continue
		}

		if err = pcState.websocket.WriteJSON(&websocketMessage{
			Event: "offer",
			Data:  string(offerJSON),
		}); err != nil {
			log.Printf("Failed to send offer: %v", err)
			continue
		}
	}
}

func websocketHandler(w http.ResponseWriter, r *http.Request) {
	unsafeConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade HTTP to WebSocket: %v", err)
		return
	}
	defer unsafeConn.Close()

	c := &threadSafeWriter{Conn: unsafeConn}

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Printf("Failed to create PeerConnection: %v", err)
		return
	}
	defer peerConnection.Close()

	// Add transceivers for video and audio
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := peerConnection.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			log.Printf("Failed to add transceiver: %v", err)
			return
		}
	}

	// Read initial message to get role and userID
	_, raw, err := c.ReadMessage()
	if err != nil {
		log.Printf("Failed to read initial message: %v", err)
		return
	}

	var initMessage websocketMessage
	if err := json.Unmarshal(raw, &initMessage); err != nil {
		log.Printf("Failed to unmarshal initial message: %v", err)
		return
	}

	if initMessage.Role == "" || initMessage.UserID == "" {
		log.Printf("Role and UserID are required")
		return
	}

	pcState := peerConnectionState{
		peerConnection: peerConnection,
		websocket:      c,
		role:           initMessage.Role,
		userID:         initMessage.UserID,
	}

	peerConnectionsMu.Lock()
	peerConnections = append(peerConnections, pcState)
	peerConnectionsMu.Unlock()

	// Remove pcState from peerConnections when done
	defer func() {
		peerConnectionsMu.Lock()
		for i, pc := range peerConnections {
			if pc == pcState {
				peerConnections = append(peerConnections[:i], peerConnections[i+1:]...)
				break
			}
		}
		peerConnectionsMu.Unlock()
	}()

	// Handle incoming tracks
	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		log.Printf("Received track from user %s: %s", pcState.userID, t.ID())

		trackType := "camera"
		if t.ID() == "screen" {
			trackType = "screen"
		}

		trackLocal, err := addTrack(t, pcState.userID, trackType)
		if err != nil {
			log.Printf("Failed to add track: %v", err)
			return
		}
		defer removeTrack(trackLocal)

		buf := make([]byte, 1500)
		rtpPkt := &rtp.Packet{}
		for {
			i, _, err := t.Read(buf)
			if err != nil {
				return
			}

			if err = rtpPkt.Unmarshal(buf[:i]); err != nil {
				log.Printf("Failed to unmarshal RTP packet: %v", err)
				continue
			}

			rtpPkt.Extension = false
			rtpPkt.Extensions = nil

			if err = trackLocal.WriteRTP(rtpPkt); err != nil {
				return
			}
		}
	})

	// Handle ICE candidates
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}

		candidateJSON, err := json.Marshal(i.ToJSON())
		if err != nil {
			log.Printf("Failed to marshal candidate: %v", err)
			return
		}

		if err = c.WriteJSON(&websocketMessage{
			Event: "candidate",
			Data:  string(candidateJSON),
		}); err != nil {
			log.Printf("Failed to send candidate: %v", err)
		}
	})

	// Handle connection state changes
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		log.Printf("Peer connection state changed: %s", p)
		if p == webrtc.PeerConnectionStateFailed || p == webrtc.PeerConnectionStateClosed {
			if err := peerConnection.Close(); err != nil {
				log.Printf("Failed to close PeerConnection: %v", err)
			}
		}
	})

	signalPeerConnections()

	// Main message loop
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			log.Printf("WebSocket read error: %v", err)
			return
		}

		var message websocketMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			log.Printf("Failed to unmarshal message: %v", err)
			return
		}

		switch message.Event {
		case "candidate":
			var candidate webrtc.ICECandidateInit
			if err := json.Unmarshal([]byte(message.Data), &candidate); err != nil {
				log.Printf("Failed to parse candidate: %v", err)
				continue
			}

			if err := peerConnection.AddICECandidate(candidate); err != nil {
				log.Printf("Failed to add ICE candidate: %v", err)
				continue
			}

		case "answer":
			var answer webrtc.SessionDescription
			if err := json.Unmarshal([]byte(message.Data), &answer); err != nil {
				log.Printf("Failed to parse answer: %v", err)
				continue
			}

			if err := peerConnection.SetRemoteDescription(answer); err != nil {
				log.Printf("Failed to set remote description: %v", err)
				continue
			}
		}
	}
}

// threadSafeWriter is a wrapper around websocket.Conn that ensures thread safety
type threadSafeWriter struct {
	*websocket.Conn
	sync.Mutex
}

func (t *threadSafeWriter) WriteJSON(v interface{}) error {
	t.Lock()
	defer t.Unlock()
	return t.Conn.WriteJSON(v)
}

// dispatchKeyFrame sends PictureLossIndication to prompt a keyframe
func dispatchKeyFrame() {
	peerConnectionsMu.Lock()
	defer peerConnectionsMu.Unlock()

	for i := range peerConnections {
		for _, receiver := range peerConnections[i].peerConnection.GetReceivers() {
			if receiver.Track() == nil {
				continue
			}
			_ = peerConnections[i].peerConnection.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(receiver.Track().SSRC()),
				},
			})
		}
	}
}
