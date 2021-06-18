package p2ptun

import (
	"context"
	"errors"
	"github.com/pion/ice"
	"github.com/pion/webrtc/v2"
	"io"
	"log"
	"net"
	"sync"
)

//SignalSender takes an sdp as string and sends it as a signal
type SignalSender func(string) error

type dataChannelWriter struct {
	*webrtc.DataChannel
}

func (s *dataChannelWriter) Write(b []byte) (int, error) {
	err := s.DataChannel.Send(b)
	return len(b), err
}

func serverConnect(addr string, conf webrtc.Configuration) (pc *webrtc.PeerConnection, ssh net.Conn, err error) {
	//create webrtc connection
	pc, err = webrtc.NewPeerConnection(conf)
	if err != nil {
		return
	}

	//create ssh connection
	ssh, err = net.Dial("tcp", addr)
	if err != nil {
		pc.Close()
		return
	}

	return
}

func serverRegisterCallbacks(pc *webrtc.PeerConnection, con net.Conn, errs chan<- error) {
	//close connections on ice disconnect
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Print("pc ice state change:", state)
		if state == ice.ConnectionStateDisconnected {
			errs <- errors.New("pc ice disconnected")
		}
	})

	//register data channel events
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		//dc.Lock()

		//write outgoing data from ssh connection to rtc data channel
		dc.OnOpen(func() {
			log.Println("rtc data channel opened")
			io.Copy(&dataChannelWriter{dc}, con)
			log.Println("data channel disconnected")
		})

		//write incoming data from rtc data channel to ssh connection
		dc.OnMessage(func(payload webrtc.DataChannelMessage) {
			log.Println("Server On Message")
			if !payload.IsString{
				_, err := con.Write(payload.Data)
				if err != nil {
					errs <- errors.New("ssh write failed: " + err.Error())
					return
				}
			}
		})

		//dc.Unlock()
	})
}

func serverHandleCommunication(ctx context.Context, addr string, conf webrtc.Configuration, SDP string, sendSignal SignalSender, wg *sync.WaitGroup) {
	//create rtc peer and local ssh conections
	defer wg.Done()
	pc, con, err := serverConnect(addr, conf)
	if err != nil {
		log.Println("connection error:", err)
		return
	}

	handleRTCError := func(err error) {
		log.Printf("server %s:\n", err)
		pc.Close()
		con.Close()
	}

	err = pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  SDP,
	})
	if err != nil {
		log.Println("ERROR SETTING REMOTE DESC")
		handleRTCError(err)
		return
	}

	//channel to receive disconnect signals from rtc callbacks
	errs := make(chan error)
	//setup rtc callback handlers
	serverRegisterCallbacks(pc, con, errs)


	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Println("ERROR CREATING ERROR")
		handleRTCError(err)
		return
	}
	err = pc.SetLocalDescription(answer)
	if err != nil {
		handleRTCError(err)
	}
	//send answer signal
	err = sendSignal(answer.SDP)
	if err != nil {
		log.Println("ERROR SENDING SIGNAL")
		handleRTCError(err)
		return
	}

	//wait for callbacks to return with error
	//or context to cancel
	waitForCommunicationEnd(ctx, errs, handleRTCError)
}

func waitForCommunicationEnd(ctx context.Context, errs <-chan error, handleError func(error)) {
	select {
	case <-ctx.Done():
		log.Println("Context ENDED")
		handleError(ctx.Err())
	case err := <-errs:
		handleError(err)
	}
}

//StartServerListener starts the ssh p2p server which listens for incomming signals
//and handles multiple simultaneous connections
func StartServerListener(ctx context.Context, addr string, conf webrtc.Configuration, sendSignal SignalSender, signals <-chan string) {
	if ctx.Err() == nil {
		log.Println("p2ptun server started")
		wg := sync.WaitGroup{}
		//wait to receive a signal
		loop:
		for {
			select {
			case SDP := <-signals:
				log.Printf("recieved connection info: %#v", SDP)
				wg.Add(1)
				go serverHandleCommunication(ctx, addr, conf, SDP, sendSignal, &wg)
			case <-ctx.Done():
				//wait for internal handlers to end
				wg.Wait()
				break loop
			}
		}
	}
	log.Println("p2ptun server stopped")
}

//StartClientListener starts listening on given TCP address and initiates a webrtc peer connection via sendSignal
func StartClientListener(ctx context.Context, addr string, conf webrtc.Configuration, sendSignal SignalSender, signals <-chan string) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalln(err)
	}
	log.Println("p2ptun client listener start:", addr)

	wg := sync.WaitGroup{}

	//listen for incomming tcp connections
	socks := make(chan net.Conn)
	wg.Add(1)
	go func(){
		defer wg.Done()
		for {
			sock, err := l.Accept()
			if err != nil {
				if ctx.Err() != nil {
					//listener was closed. Ignore error and end loop
					return
				}
				log.Println(err)
				continue
			}
			socks<- sock
		}
	}()

	for {
		select {
		case <-ctx.Done():
			err := l.Close()
			if err != nil {
				log.Printf("error closing tcp listener: %v: %s\n", addr, err)
			}
			wg.Wait()
			log.Println("p2ptun listener closed:", addr)
			return
		case sock := <-socks:
			wg.Add(1)
			go clientHandleCommunication(ctx, conf, sock, sendSignal, signals, &wg)
		default:

		}
	}
}

func clientSetupDataChannel(pc *webrtc.PeerConnection, sock net.Conn, errs chan<- error) (*webrtc.DataChannel, error) {

	dc, err := pc.CreateDataChannel("data", nil)
	if err != nil {
		return nil, err
	}
	//dc.Lock()
	dc.OnOpen(func() {
		log.Println("client data channel open")
		io.Copy(&dataChannelWriter{dc}, sock)
		errs <- errors.New("datachannel EOF")
	})
	dc.OnMessage(func(payload webrtc.DataChannelMessage) {
		if !payload.IsString {
			_, err := sock.Write(payload.Data)
			if err != nil {
				errs <- errors.New("sock write failed:" + err.Error())
			}
		}
	})
	return dc, nil
}

func clientWaitForAnswer(ctx context.Context, pc *webrtc.PeerConnection, signals <-chan string, errs chan<- error, wg *sync.WaitGroup) {
	defer wg.Done()

	//ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	//defer cancel()

	select {
	case <-ctx.Done():
		errs <- ctx.Err()
	case SDP := <-signals:
		log.Printf("info: %#v", SDP)
		err := pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  SDP,
		})
		if err != nil {
			errs <- err
		}
	}
}

func clientHandleCommunication(ctx context.Context, conf webrtc.Configuration, sock net.Conn, sendSignal SignalSender, signals <-chan string, wg *sync.WaitGroup) {
	defer wg.Done()
	pc, err := webrtc.NewPeerConnection(conf)
	if err != nil {
		log.Println("rtc error:", err)
		return
	}
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Print("pc ice state change:", state)
	})

	handleErr := func(err error) {
		log.Println(err)
		pc.Close()
	}

	//connect rtc data channel to incomming tcp sock
	errs := make(chan error)
	dc, err := clientSetupDataChannel(pc, sock, errs)
	if err != nil {
		handleErr(err)
	}
	log.Println("DataChannel:", dc)

	//dc.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	//cancel context if there is an error sending offer
	defer cancel()

	//listen for peer signal answer
	wg.Add(1)
	go clientWaitForAnswer(ctx, pc, signals, errs, wg)

	//create and send offer to peer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		handleErr(err)
		return
	}
	err = pc.SetLocalDescription(offer)
	if err != nil {
		handleErr(err)
	}
	if err := sendSignal(offer.SDP); err != nil {
		handleErr(err)
		return
	}

	//wait until either connections are disrupted or parent context cancels
	waitForCommunicationEnd(ctx, errs, handleErr)
}
