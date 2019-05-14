// Copyright (c) 2019, The Fonero developers
// See LICENSE for details.

package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/fonero-project/fnod/fnoutil"
	exptypes "github.com/fonero-project/fnodata/explorer/types"
	client "github.com/fonero-project/fnodata/pubsub/psclient"
	pstypes "github.com/fonero-project/fnodata/pubsub/types"
	"golang.org/x/net/websocket"
	survey "gopkg.in/AlecAivazis/survey.v1"
)

var cfg *config

func main() {
	var err error
	cfg, err = loadConfig()
	if err != nil {
		log.Fatalf("%v", err)
		return
	}

	// Create the websocket connection.
	origin := "/"
	ws, err := websocket.Dial(cfg.URL, "", origin)
	if err != nil {
		log.Fatalf("%v", err)
		return
	}
	defer ws.Close()

	fmt.Printf("You are now connected to %s.\n", cfg.URL)

	// Create the pubsub client.
	cl := client.New(ws)
	cl.ReadTimeout = 3 * time.Second
	cl.WriteTimeout = 3 * time.Second

	// Subscribe/unsubscribe to several events.
	var currentSubs []string
	allSubs := []string{"ping", "newtxs", "newblock", "mempool", "address:Dcur2mcGjmENx4DhNqDctW5wJCVyT3Qeqkx", "address"}
	subscribe := func(newsubs []string) error {
		for _, sub := range newsubs {
			if subd, _ := strInSlice(currentSubs, sub); subd {
				log.Printf("Already subscribed to %s.", sub)
				continue
			}
			currentSubs = append(currentSubs, sub)
			resp, err := cl.Subscribe(sub)
			if err != nil {
				return fmt.Errorf("Failed to subscribe: %v", err)
			}
			log.Printf(resp.Message)
		}
		return nil
	}
	unsubscribe := func(rmsubs []string) error {
		for _, sub := range rmsubs {
			subd, i := strInSlice(currentSubs, sub)
			if !subd {
				log.Printf("Not subscribed to %s.", sub)
				continue
			}
			currentSubs = append(currentSubs[:i], currentSubs[i+1:]...)
			resp, err := cl.Unsubscribe(sub)
			if err != nil {
				return fmt.Errorf("Failed to unsubscribe: %v", err)
			}
			log.Printf(resp.Message)
		}
		return nil
	}

	// Prompts
	type actionData struct {
		action string
		data   []string
	}
	actionChan := make(chan *actionData, 1)
	promptAgain := make(chan struct{})

	subPrompt := &survey.MultiSelect{
		Message: "Subscribe to events:",
		Options: allSubs,
	}
	unsubPrompt := &survey.MultiSelect{
		Message: "Unsubscribe to events:",
		Options: allSubs,
	}

	hitEnter := func() {
		fmt.Println("Hit a key to choose an action.")
		//bufio.NewReaderSize(os.Stdin, 1).ReadByte()
		os.Stdin.Read([]byte{0})
	}

	// Prompting goroutine that sends the sub/unsub requests to the message loop
	go func() {
		for range promptAgain {
			hitEnter()
			var a actionData
			actionPrompt := &survey.Select{
				Message: "What now?",
				Options: []string{"subscribe", "unsubscribe", "quit"},
			}
			err := survey.AskOne(actionPrompt, &a.action, nil)
			if err != nil {
				log.Fatal(err)
				go func() { promptAgain <- struct{}{} }()
				continue
			}

			switch a.action {
			case "subscribe":
				subPrompt.Default = AnotInB(allSubs, append(currentSubs, "address"))
				_ = survey.AskOne(subPrompt, &a.data, nil)
				data := make([]string, 0, len(a.data))
				for i := range a.data {
					if a.data[i] == "address" {
						var addr string
						err = survey.AskOne(&survey.Input{Message: "Type the address."}, &addr, nil)
						if err != nil {
							log.Fatal(err)
							continue
						}
						_, err = fnoutil.DecodeAddress(addr)
						if err != nil {
							log.Fatalf("Invalid address %s: %v", addr, err)
							continue
						}

						data = append(data, a.data[i]+":"+addr)
					} else {
						data = append(data, a.data[i])
					}
				}
				a.data = data
			case "unsubscribe":
				unsubPrompt.Options = currentSubs
				_ = survey.AskOne(unsubPrompt, &a.data, nil)
			case "quit":
				close(promptAgain)
				os.Exit(0)
			default:
				log.Fatalf("invalid action")
				continue
			}

			if len(a.data) == 0 {
				//actionChan <- &actionData{"", nil}
				go func() { promptAgain <- struct{}{} }()
				continue
			}

			log.Printf("Submitting %s request...", a.action)
			actionChan <- &a
		}
	}()
	promptAgain <- struct{}{}

	// Send/receive messages in an orderly fashion.
	for {
		select {
		case a := <-actionChan:
			switch a.action {
			case "subscribe":
				if err = subscribe(a.data); err != nil {
					log.Fatalf("subscribed failed: %v", err)
				}
			case "unsubscribe":
				if err = unsubscribe(a.data); err != nil {
					log.Fatalf("subscribed failed: %v", err)
				}
			case "quit":
				close(promptAgain)
				close(actionChan)
				os.Exit(0)
			}

			promptAgain <- struct{}{}
		default:
			//log.Println("No actions received. Going on to wait for messages.")
		}

		resp, err := cl.ReceiveMsg()
		if err != nil {
			if pstypes.IsIOTimeoutErr(err) {
				continue
			}
			fmt.Printf("ReceiveMsg failed: %v", err)
			return
		}

		msg, err := client.DecodeMsg(resp)
		if err != nil {
			log.Printf("Failed to decode message: %v", err)
			continue
		}

		switch m := msg.(type) {
		case string:
			log.Printf("Message (%s): %s", resp.EventId, m)
		case *exptypes.WebsocketBlock:
			log.Printf("Message (%s): WebsocketBlock(hash=%s)", resp.EventId, m.Block.Hash)
		case *exptypes.MempoolShort:
			t := time.Unix(m.Time, 0)
			log.Printf("Message (%s): MempoolShort(numTx=%d, time=%v)",
				resp.EventId, m.NumAll, t)
		case *pstypes.TxList:
			log.Printf("Message (%s): TxList(len=%d)", resp.EventId, len(*m))
		case *pstypes.AddressMessage:
			log.Printf("Message (%s): AddressMessage(address=%s, txHash=%s)",
				resp.EventId, m.Address, m.TxHash)
		default:
			log.Printf("Message of type %v unhandled.", resp.EventId)
		}
	}
}

func strInSlice(sl []string, str string) (bool, int) {
	for i, s := range sl {
		if s == str {
			return true, i
		}
	}
	return false, -1
}

// AnotInB returns strings in the slice sA that are not in slice sB.
func AnotInB(sA []string, sB []string) (AnotB []string) {
	for _, s := range sA {
		if found, _ := strInSlice(sB, s); found {
			continue
		}
		AnotB = append(AnotB, s)
	}
	return
}
