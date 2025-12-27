package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "example.com/chatty/chat/chatpb"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "server address")
	name := flag.String("name", "", "your name")
	room := flag.String("room", "room1", "room name")
	flag.Parse()

	if *name == "" {
		log.Fatal("missing -name")
	}

	conn, err := grpc.Dial(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	c := pb.NewChatServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chatStream, err := c.Chat(ctx)
	if err != nil {
		log.Fatal(err)
	}

	// Send join message first.
	// You can omit UnixMs, the server overwrites it anyway.
	if err := chatStream.Send(&pb.ChatMessage{
		Room: *room,
		From: *name,
		Text: "",
	}); err != nil {
		log.Fatal(err)
	}

	var printMu sync.Mutex
	safePrintf := func(format string, args ...any) {
		printMu.Lock()
		defer printMu.Unlock()
		fmt.Printf(format, args...)
	}

	// Start WatchRooms stream for live stats.
	go func() {
		statsStream, err := c.WatchRooms(ctx, &pb.RoomsRequest{})
		if err != nil {
			safePrintf("[stats] watch error: %v\n", err)
			return
		}

		for {
			snap, err := statsStream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				safePrintf("[stats] recv error: %v\n", err)
				return
			}

			printRoomsSnapshot(safePrintf, snap)
		}
	}()

	// Receiver goroutine for chat.
	go func() {
		for {
			in, err := chatStream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				safePrintf("recv error: %v\n", err)
				return
			}

			// Skip the empty join echo from yourself if your server ever echoes it.
			if in.GetFrom() == *name && in.GetText() == "" {
				continue
			}

			if in.GetText() == "" {
				safePrintf("[%s] %s\n", in.GetRoom(), in.GetFrom())
				continue
			}
			safePrintf("[%s] %s: %s\n", in.GetRoom(), in.GetFrom(), in.GetText())
		}
	}()

	safePrintf("type messages then press enter. type /quit to exit\n")

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := sc.Text()
		if line == "/quit" {
			_ = chatStream.CloseSend()
			cancel()
			return
		}

		if err := chatStream.Send(&pb.ChatMessage{
			Room: *room,
			From: *name,
			Text: line,
		}); err != nil {
			safePrintf("send error: %v\n", err)
			cancel()
			return
		}
	}
}

func printRoomsSnapshot(printf func(string, ...any), snap *pb.RoomsSnapshot) {
	rooms := snap.GetRooms()

	// Build a stable, sorted view.
	type roomView struct {
		name    string
		clients []string
	}
	views := make([]roomView, 0, len(rooms))

	totalClients := 0
	for _, r := range rooms {
		names := append([]string(nil), r.GetClients()...)
		sort.Strings(names)
		totalClients += len(names)
		views = append(views, roomView{name: r.GetRoom(), clients: names})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].name < views[j].name })

	// Timestamp.
	t := "--:--:--"
	if snap.GetUnixMs() > 0 {
		ts := time.UnixMilli(snap.GetUnixMs()).Local()
		t = ts.Format("15:04:05")
	}

	printf("\n[stats %s] active rooms=%d active clients=%d\n", t, len(views), totalClients)
	if len(views) == 0 {
		printf("[stats] no active rooms\n")
		printf("\n")
		return
	}

	for _, v := range views {
		if len(v.clients) == 0 {
			printf("[stats] %s: (no clients)\n", v.name)
			continue
		}
		printf("[stats] %s: %s\n", v.name, strings.Join(v.clients, ", "))
	}
	printf("\n")
}
