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

	// watchers for WatchRooms
	nextWatcherID int
	watchers      map[int]chan *pb.RoomsSnapshot
}

func newServer() *server {
	return &server{
		rooms:    map[string]*roomState{},
		watchers: map[int]chan *pb.RoomsSnapshot{},
	}
}

func (s *server) Chat(stream pb.ChatService_ChatServer) error {
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
	s.notifyRoomsChanged() // NEW
	defer func() {
		s.removeClient(c)
		s.notifyRoomsChanged() // NEW
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range c.send {
			if err := stream.Send(msg); err != nil {
				return
			}
		}
	}()

	s.broadcast(c.room, &pb.ChatMessage{
		Room:   c.room,
		From:   "server",
		Text:   fmt.Sprintf("%s joined", c.name),
		UnixMs: time.Now().UnixMilli(),
	}, nil)

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

		if s.roomSize(c.room) > 2 {
			c.send <- &pb.ChatMessage{
				Room:   c.room,
				From:   "server",
				Text:   "room already has 2 clients",
				UnixMs: time.Now().UnixMilli(),
			}
			continue
		}

		s.broadcast(c.room, msg, c)
	}

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
	if rs == nil {
		s.mu.Unlock()
		return
	}
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
		}
	}
}

func (s *server) WatchRooms(req *pb.RoomsRequest, stream pb.ChatService_WatchRoomsServer) error {
	ch, id := s.addWatcher()
	defer s.removeWatcher(id)

	// Send initial snapshot immediately.
	if err := stream.Send(s.buildRoomsSnapshot()); err != nil {
		return err
	}

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case snap, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(snap); err != nil {
				return err
			}
		}
	}
}

func (s *server) addWatcher() (chan *pb.RoomsSnapshot, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextWatcherID++
	id := s.nextWatcherID
	ch := make(chan *pb.RoomsSnapshot, 8)
	s.watchers[id] = ch
	return ch, id
}

func (s *server) removeWatcher(id int) {
	s.mu.Lock()
	ch := s.watchers[id]
	delete(s.watchers, id)
	s.mu.Unlock()

	if ch != nil {
		close(ch)
	}
}

func (s *server) notifyRoomsChanged() {
	snap := s.buildRoomsSnapshot()

	s.mu.Lock()
	watchers := make([]chan *pb.RoomsSnapshot, 0, len(s.watchers))
	for _, ch := range s.watchers {
		watchers = append(watchers, ch)
	}
	s.mu.Unlock()

	for _, ch := range watchers {
		select {
		case ch <- snap:
		default:
			// Drop if watcher is slow.
		}
	}
}

func (s *server) buildRoomsSnapshot() *pb.RoomsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	rooms := make([]*pb.RoomInfo, 0, len(s.rooms))
	for roomName, rs := range s.rooms {
		names := make([]string, 0, len(rs.clients))
		for c := range rs.clients {
			names = append(names, c.name)
		}
		rooms = append(rooms, &pb.RoomInfo{
			Room:    roomName,
			Clients: names,
		})
	}

	return &pb.RoomsSnapshot{
		UnixMs: time.Now().UnixMilli(),
		Rooms:  rooms,
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
