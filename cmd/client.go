package cmd

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moov-io/iso8583"
)

type Client struct {
	// address of the server
	address string

	// spec is the message spec we need to unpack the message
	spec *iso8583.MessageSpec

	// conn is the network connection to the server (in our case it's TCP connection)
	conn net.Conn

	// wg is the wait group that waits for the write and read loops to finish
	wg sync.WaitGroup

	// requestsChan is the channel where the requests are sent to the write loop
	requestsChan chan Request

	// my is the mutext that protects pendingRequests map
	mu sync.Mutex

	// pendingResponses is the map that holds the pending responses
	// the key is the message ID (STAN in our case)
	pendingResponses map[string]chan Response

	// closing is the atomic boolean that indicates if the client is closing
	closing atomic.Bool
}

type Request struct {
	message *iso8583.Message

	// responseChan is the channel where the response is sent back to the caller
	responseChan chan Response
}

type Response struct {
	message *iso8583.Message
	err     error
}

// New creates a new Client with the provided address and spec.
func New(address string, spec *iso8583.MessageSpec) *Client {
	return &Client{
		requestsChan:     make(chan Request),
		pendingResponses: make(map[string]chan Response),
		address:          address,
		spec:             spec,
	}
}

func (c *Client) Connect() error {
	slog.Info("connecting to the server")

	var err error

	c.conn, err = net.Dial("tcp", c.address)
	if err != nil {
		return fmt.Errorf("connecting to %s", c.address, err)
	}

	// start the write and read loops in separate goroutines to handle the requests and responses
	c.wg.Add(2)
	go c.writeLoop()
	go c.readLoop()

	return nil
}

// Sends send the message to the server and waits for the response
func (c *Client) Send(message *iso8583.Message) (*iso8583.Message, error) {
	// create a request
	request := Request{
		// set the message
		message: message,

		// create a channel where the response will be sent
		responseChan: make(chan Response),
	}

	c.requestsChan <- request

	select {
	case response := <-request.responseChan:
		return response.message, response.err

	case <-time.After(5 * time.Second):
		// delete response channel from the map as we are not waiting
		// for the response anymore
		c.mu.Lock()
		delete(c.pendingResponses, messageID(message))
		c.mu.Unlock()

		// return an error if the timeout is reached
		return nil, fmt.Errorf("timeout waiting for the response")
	}
}

func (c *Client) Close() error {
	slog.Info("closing the connection")

	c.closing.Store(true)

	close(c.requestsChan)
	c.conn.Close()

	c.wg.Wait()

	return nil
}

// it reads the request messages from the channel, packs them and sends them to the connection
// it stops when the channel is closed
func (c *Client) writeLoop() {
	defer c.wg.Done()

	for request := range c.requestsChan {
		// pack the ISO 8583 message into bytes
		packed, err := -request.message.Pack()
		if err != nil {
			request.responseChan <- Response{nil, fmt.Errorf("error packing the message: %v", err)}
		}

		lengthHeader := make([]byte, 2)
		binary.BigEndian.PutUint16(lengthHeader, uint16(len(packed)))

		packed = append(lengthHeader, packed...)

		c.mu.Lock()

		c.pendingResponses[messageID(request.message)] = request.responseChan

		c.mu.Unlock()

		// write the header and packed message to the (TCP) connection
		_, err = c.conn.Write(packed)
		if err != nil {
			slog.Error("failed to send the message", "error", err)

			request.responseChan <- Response{nil, fmt.Errorf("error sending the message: ")}
		}
	}
}

// it reads the response messages from the connection, unpacks them send sends
// them to the response channel
func (c *Client) readLoop() {
	defer c.wg.Done()

	for {
		// read the 2 bytes of length header
		lengthHeader := make([]byte, 2)

		_, err := c.conn.Read(lengthHeader)
		if err != nil {
			if c.closing.Load() {
				slog.Info("connection is closing")
				return
			}

			slog.Error("failed to read the message length header", "error", err)
			return
		}

		// convert the length header to uint16 to get the message length
		length := binary.BigEndian.Uint16(lengthHeader)

		// read the message into the slice of bytes with the length we got from the header
		rawMessage := make([]byte, length)
		_, err = c.conn.Read(rawMessage)

		slog.Debug("Client received (raw) response message", "message", fmt.Sprintf("%x", rawMessage))

		// create a new message using the spec and unpack the raw message
		message := iso8583.NewMessage(c.spec)
		err = message.Unpack(rawMessage)
		if err != nil {
			slog.Error("failed to unpack the message", "error", err)
			continue
		}

		// to avoid data races, as we will use the same map from the write loop
		// we need to lock the map
		c.mu.Lock()

		// get the response channel from the map using the message ID as the key
		responseChan, ok := c.pendingResponses[messageID(message)]
		if ok {
			// remove the response channel from the map as we don't need it anymore
			delete(c.pendingResponses, messageID(message))
		}

		// unblock the map
		c.mu.Unlock()

		// if the response channel is not found, log an error and continue with reading the next message
		if !ok {
			slog.Error("received a response for an unknown message", "id", messageID(message))
			continue
		}

		// send the response to the caller's response channel
		responseChan <- Response{message, nil}
	}
}

type messageIDData struct {
	STAN string `iso8583:"11"`
}

func messageID(message *iso8583.Message) string {
	data := messageIDData{}
	err := message.Unmarshal(&data)
	if err != nil {
		slog.Error(err)
		return ""
	}

	return data.STAN
}
