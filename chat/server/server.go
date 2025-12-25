package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"

	pb "example.com/chatty/chat/chatpb"
)

type client struct {
	name   string
	room   string
	stream pb.ChatService_ChatServer
	send   chan *pb.ChatMessage
}

type roomState struct {
	clients map[*client]struct{}
}

type server struct {
	pb.UnimplementedChatServiceServer

	mu    sync.Mutex
	rooms map[string]*roomState
}

func newServer() *server {
	return &server{
		rooms: map[string]*roomState{},
	}
}

func (s *server) Chat(stream pb.ChatService_ChatServer) error {
	// First message is treated as join info.
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetRoom() == "" || first.GetFrom() == "" {
		return fmt.Errorf("room and from are required in first message")
	}

	c := &client{
		name:   first.GetFrom(),
		room:   first.GetRoom(),
		stream: stream,
		send:   make(chan *pb.ChatMessage, 32),
	}

	if p, ok := peer.FromContext(stream.Context()); ok {
		log.Printf("client connected name=%s room=%s addr=%s", c.name, c.room, p.Addr)
	} else {
		log.Printf("client connected name=%s room=%s", c.name, c.room)
	}

	s.addClient(c)
	defer s.removeClient(c)

	// Start sender loop.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range c.send {
			if err := stream.Send(msg); err != nil {
				return
			}
		}
	}()

	// Broadcast join info.
	s.broadcast(c.room, &pb.ChatMessage{
		Room:   c.room,
		From:   "server",
		Text:   fmt.Sprintf("%s joined", c.name),
		UnixMs: time.Now().UnixMilli(),
	}, nil)

	// Receive loop.
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if msg.GetRoom() == "" {
			msg.Room = c.room
		}
		if msg.GetFrom() == "" {
			msg.From = c.name
		}
		msg.UnixMs = time.Now().UnixMilli()

		// Enforce 2 client chat. If more than 2 are in room, reject message.
		if s.roomSize(c.room) > 2 {
			c.send <- &pb.ChatMessage{
				Room:   c.room,
				From:   "server",
				Text:   "room already has 2 clients",
				UnixMs: time.Now().UnixMilli(),
			}
			continue
		}

		// Relay to everyone else in the room.
		s.broadcast(c.room, msg, c)
	}

	// Stop sender.
	s.broadcast(c.room, &pb.ChatMessage{
		Room:   c.room,
		From:   "server",
		Text:   fmt.Sprintf("%s left", c.name),
		UnixMs: time.Now().UnixMilli(),
	}, c)

	s.closeClient(c)
	<-done
	return nil
}

func (s *server) addClient(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rs := s.rooms[c.room]
	if rs == nil {
		rs = &roomState{clients: map[*client]struct{}{}}
		s.rooms[c.room] = rs
	}
	rs.clients[c] = struct{}{}
}

func (s *server) removeClient(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rs := s.rooms[c.room]
	if rs == nil {
		return
	}
	delete(rs.clients, c)
	if len(rs.clients) == 0 {
		delete(s.rooms, c.room)
	}
}

func (s *server) closeClient(c *client) {
	// Safe close.
	defer func() { recover() }()
	close(c.send)
}

func (s *server) roomSize(room string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.rooms[room]
	if rs == nil {
		return 0
	}
	return len(rs.clients)
}

func (s *server) broadcast(room string, msg *pb.ChatMessage, exclude *client) {
	s.mu.Lock()
	rs := s.rooms[room]
	clients := make([]*client, 0, len(rs.clients))
	for cl := range rs.clients {
		if exclude != nil && cl == exclude {
			continue
		}
		clients = append(clients, cl)
	}
	s.mu.Unlock()

	for _, cl := range clients {
		select {
		case cl.send <- msg:
		default:
			// Drop if receiver is slow.
		}
	}
}

func main() {
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatal(err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterChatServiceServer(grpcServer, newServer())

	log.Println("server listening on :50051")
	log.Fatal(grpcServer.Serve(lis))
}
