package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
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
	ctx := context.Background()

	stream, err := c.Chat(ctx)
	if err != nil {
		log.Fatal(err)
	}

	// Send join message first.
	if err := stream.Send(&pb.ChatMessage{
		Room:   *room,
		From:   *name,
		Text:   "",
		UnixMs: time.Now().UnixMilli(),
	}); err != nil {
		log.Fatal(err)
	}

	// Receiver goroutine.
	go func() {
		for {
			in, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				log.Printf("recv error: %v", err)
				return
			}
			if in.GetFrom() == *name && in.GetText() == "" {
				continue
			}
			if in.GetText() == "" {
				fmt.Printf("[%s] %s\n", in.GetRoom(), in.GetFrom())
				continue
			}
			fmt.Printf("[%s] %s: %s\n", in.GetRoom(), in.GetFrom(), in.GetText())
		}
	}()

	fmt.Println("type messages then press enter. type /quit to exit")

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := sc.Text()
		if line == "/quit" {
			_ = stream.CloseSend()
			return
		}
		if err := stream.Send(&pb.ChatMessage{
			Room: *room,
			From: *name,
			Text: line,
		}); err != nil {
			log.Printf("send error: %v", err)
			return
		}
	}
}
