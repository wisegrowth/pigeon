package scheduler

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/iampigeon/pigeon"
	"github.com/iampigeon/pigeon/db"
	pb "github.com/iampigeon/pigeon/proto"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

// TODO(ja): remove this struct.

// StorageConfig is a struct that will be deleted.
type StorageConfig struct {
	// BoltDatabase     string        // File to use as bolt database.
	RedisURL         string        // URL of the redis server
	RedisLog         bool          // log database commands
	RedisMaxIdle     int           // maximum number of idle connections in the pool
	RedisDatabase    int           // redis database to use
	RedisIdleTimeout time.Duration // timeout for idle connections

	MessageStore *db.MessageStore
}

// NewStoreBackend builds a new pigeon.Store backed by bolt DB.
//
// In case of any error it panics.
func NewStoreBackend(config StorageConfig) (pigeon.SchedulerService, error) {
	pq, err := newPriorityQueue(config)
	if err != nil {
		return nil, err
	}

	s := &service{
		pq:  pq,
		idc: make(chan ulid.ULID),

		ms: config.MessageStore,
	}

	go s.run()

	return s, nil
}

var msgBucket = []byte("messages")

type service struct {
	// db *bolt.DB
	pq *priorityQueue

	idc chan ulid.ULID

	ms *db.MessageStore
}

func (s *service) Put(id ulid.ULID, content []byte, endpoint pigeon.NetAddr, status pigeon.MessageStatus, subjectID string) error {
	// TODO(ja): use secure connections

	host, port, err := net.SplitHostPort(string(endpoint))
	if err != nil {
		return err
	}

	endpoint = pigeon.NetAddr(net.JoinHostPort(host, port))
	log.Println(endpoint)

	conn, err := grpc.Dial(string(endpoint), grpc.WithInsecure())
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pb.NewBackendServiceClient(conn)
	resp, err := client.Approve(context.Background(), &pb.ApproveRequest{Content: content})
	if err != nil {
		// update status to crashed-approve
		e := s.ms.UpdateStatus(id, pigeon.StatusCrashedApprove)
		if e != nil {
			return e
		}

		return err
	}
	if !resp.Valid {
		// update status to failed-approve
		e := s.ms.UpdateStatus(id, pigeon.StatusFailedApprove)
		if e != nil {
			return e
		}

		if resp.Error != nil {
			return errors.Errorf("invalid message, %s", resp.Error.Message)
		}
		return errors.New("invalid message")
	}

	m := pigeon.Message{
		ID:        id,
		Content:   content,
		Endpoint:  endpoint,
		Status:    status,
		SubjectID: subjectID,
	}

	err = s.ms.AddMessage(m)
	if err != nil {
		return err
	}

	s.idc <- id

	return nil
}

func (s *service) deleteByID(id string) error {
}

func (s *service) Get(id ulid.ULID) (*pigeon.Message, error) {
	msg, err := s.ms.GetMessage(id)
	if err != nil {
		return nil, err
	}

	return msg, nil
}

// TODO(ca): change Update protobuf params method
func (s *service) Update(id ulid.ULID, content []byte) error {
	err := s.ms.UpdateContent(id, content)
	if err != nil {
		return err
	}

	return nil
}

// Run in its goroutine
func (s *service) run() {
	var next uint64
	var timer *time.Timer

	pq := s.pq
	for {
		var tick <-chan time.Time

		top, err := pq.Peek()
		if err != nil {
			log.Println(err)
			return
		}
		if top != nil {
			if t := top.Time(); t < next || next == 0 {
				var delay int64
				now := ulid.Timestamp(time.Now())
				if t >= now {
					delay = int64(t - now)
				}

				if timer == nil {
					timer = time.NewTimer(time.Duration(delay) * time.Millisecond)
				} else {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer = time.NewTimer(time.Duration(delay) * time.Millisecond)
				}
			}
		}

		if timer != nil && top != nil {
			tick = timer.C
		}

		select {
		case <-tick:
			id, err := pq.Pop()
			if err != nil {
				log.Println(err)
			}
			if id != nil {
				go s.send(*id)
			}
			next = 0
		case id := <-s.idc:
			pq.Push(id)
		}
	}
}

func (s *service) send(id ulid.ULID) {
	msg, err := s.Get(id)
	if err != nil {
		log.Printf("Error: could not get message %s, %v", id, err)
		return
	}

	// TODO(ja): use secure connections
	conn, err := grpc.Dial(string(msg.Endpoint), grpc.WithInsecure())
	if err != nil {
		log.Printf("Error: could not connect to backend at %s, %v", msg.Endpoint, err)
		return
	}
	defer conn.Close()

	client := pb.NewBackendServiceClient(conn)
	// TODO(ja): handle cancellation.
	resp, err := client.Deliver(context.Background(), &pb.DeliverRequest{Content: msg.Content})
	if err != nil {
		log.Printf("Error: could not deliver message %s, %v", msg.ID, err)

		// update status to crashed-deliver
		e := s.ms.UpdateStatus(id, pigeon.StatusCrashedDeliver)
		if e != nil {
			log.Printf("Error: could not update message status %s, %v", msg.ID, err)
			return
		}

		return
	}
	if resp.Error != nil {
		log.Printf("Error: failed to deliver message %s, %v", msg.ID, resp.Error.Message)

		// update status to failed-deliver
		e := s.ms.UpdateStatus(id, pigeon.StatusFailedDeliver)
		if e != nil {
			log.Printf("Error: could not update message status %s, %v", msg.ID, err)
			return
		}

		return
	}

	e := s.ms.UpdateStatus(id, pigeon.StatusSent)
	if e != nil {
		log.Printf("Error: could not update message status %s, %v", msg.ID, err)
		return
	}

	// TODO(ca): send Put Message with 'callback_post_url' message to pigeon-http
}
