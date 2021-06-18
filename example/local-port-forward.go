package main

import (
	"context"
	"fmt"
	"github.com/justinsantoro/p2ptun"
	"github.com/pion/webrtc/v2"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	wg := sync.WaitGroup{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "PONG!: received ping from: ", r.RemoteAddr)
	})
	srv := http.Server{
		Addr: ":8080",
		Handler: nil,
	}
	log.Println("starting http server")
	wg.Add(2)
	go func(){
		defer wg.Done()
		go func(){
			defer wg.Done()
			_ = srv.ListenAndServe()
		}()
		<-ctx.Done()
		log.Println("stopping http server")
		ctx, cancel = context.WithTimeout(context.Background(), 5 * time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{},
	}

	//create signal channels
	clientSignals := make(chan string)
	serverSignals := make(chan string)

	//create signal senders
	clientSignalSender := func(offer string) error {
		clientSignals<- offer
		return nil
	}

	serverSignalSender := func(answer string) error {
		serverSignals<- answer
		return nil
	}

	//start listeners
	//local port to listen to packets to be forwarded
	log.Println("starting client listener")
	wg.Add(1)
	go func(){
		defer wg.Done()
		p2ptun.StartClientListener(ctx, ":445", config, clientSignalSender, serverSignals)
	}()

	log.Println("Starting server listener")
	//port to forward packets to (same as http server)
	wg.Add(1)
	go func(){
		defer wg.Done()
		p2ptun.StartServerListener(ctx, ":8080", config, serverSignalSender, clientSignals)
	}()

	//wait for user SIGINT
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT)
	log.Println("waiting")
	<-sig
	//signal all goroutines to end gracefully
	cancel()
	//wait for goroutines to end
	wg.Wait()
}
