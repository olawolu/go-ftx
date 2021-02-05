package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"

	"github.com/uscott/go-ftx/models"
)

const (
	wsUrl                 = "wss://ftx.com/ws/"
	websocketTimeout      = time.Second * 60
	pingPeriod            = (websocketTimeout * 9) / 10
	reconnectCount    int = 10
	reconnectInterval     = time.Second
)

type Stream struct {
	client                 *Client
	mu                     *sync.Mutex
	url                    string
	dialer                 *websocket.Dialer
	wsReconnectionCount    int
	wsReconnectionInterval time.Duration
	isDebugMode            bool
	Subs                   []*WsSub
}

type WsSub struct {
	Conn        *websocket.Conn
	ChannelType models.ChannelType
	EventC      chan interface{}
	Symbols     []string
}

func MakeRequests(
	chantype models.ChannelType, symbols ...string) []models.WSRequest {

	if len(symbols) == 0 {
		return []models.WSRequest{
			{ChannelType: chantype, Op: models.Subscribe},
		}
	}

	requests := make([]models.WSRequest, len(symbols))

	for i, s := range symbols {
		requests[i] = models.WSRequest{
			ChannelType: chantype,
			Market:      s,
			Op:          models.Subscribe,
		}
	}

	return requests
}

func (s *Stream) Authorize(conn *websocket.Conn) (err error) {

	if conn == nil {
		return fmt.Errorf("Nil websocket pointer")
	}

	ms := time.Now().UTC().UnixNano() / int64(time.Millisecond)
	mac := hmac.New(sha256.New, []byte(s.client.secret))

	_, err = mac.Write([]byte(fmt.Sprintf("%dwebsocket_login", ms)))
	if err != nil {
		return errors.WithStack(err)
	}

	args := map[string]interface{}{
		"key":  s.client.apiKey,
		"sign": hex.EncodeToString(mac.Sum(nil)),
		"time": ms,
	}

	if s.client.SubAccount != nil {
		args["subaccount"] = *s.client.SubAccount
	}

	err = conn.WriteJSON(&models.WSRequestAuthorize{
		Op:   "login",
		Args: args,
	})
	if err != nil {
		return errors.WithStack(err)
	}

	return
}

func (s *Stream) Connect(conn *websocket.Conn, requests ...models.WSRequest) (err error) {

	if conn == nil {
		return fmt.Errorf("Nil websocket pointer")
	}
	s.printf("connected to %v", s.url)

	if err = s.Subscribe(conn, requests); err != nil {
		return errors.WithStack(err)
	}

	lastPong := time.Now()
	conn.SetPongHandler(
		func(msg string) error {
			lastPong = time.Now()
			if time.Since(lastPong) > websocketTimeout {
				// TODO handle this case
				errmsg := "PONG response time has been exceeded"
				s.printf(errmsg)
				return fmt.Errorf(errmsg) // Handled?
			} else {
				s.printf("PONG")
			}
			return nil
		})
	return nil
}

func (s *Stream) CreateNewConnection() (conn *websocket.Conn, err error) {

	conn, _, err = s.dialer.Dial(s.url, nil)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return
}

func (s *Stream) GetEventResponse(
	ctx context.Context,
	conn *websocket.Conn,
	eventsC chan interface{},
	msg *models.WsResponse,
	requests ...models.WSRequest) (err error) {

	err = conn.ReadJSON(&msg)

	if err != nil {

		s.printf("read msg: %v", err)

		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			return
		}

		err = s.Reconnect(ctx, conn, requests)
		if err != nil {
			s.printf("reconnect: %+v", err)
			return
		}

		return nil
	}

	if msg.ResponseType == models.Subscribed || msg.ResponseType == models.UnSubscribed {
		return
	}

	var response interface{}

	switch msg.ChannelType {
	case models.TickerChannel:
		response, err = msg.MapToTickerResponse()
	case models.TradesChannel:
		response, err = msg.MapToTradesResponse()
	case models.OrderBookChannel:
		response, err = msg.MapToOrderBookResponse()
	case models.MarketsChannel:
		response = msg.Data
	case models.FillsChannel:
		response, err = msg.MapToFillResponse()
	case models.OrdersChannel:
		response, err = msg.MapToOrdersResponse()
	}

	eventsC <- response

	return
}

func (s *Stream) GetEventsChannel(
	ctx context.Context,
	conn *websocket.Conn,
	ct models.ChannelType,
	symbols ...string) (eventC chan interface{}, err error) {

	requests := MakeRequests(ct, symbols...)
	if err = s.Subscribe(conn, requests); err != nil {
		return
	}

	if eventC, err = s.Serve(ctx, conn, requests...); err != nil {
		return
	}

	return
}

func (s *Stream) Reconnect(
	ctx context.Context, conn *websocket.Conn, requests []models.WSRequest) (err error) {

	for i := 0; i < s.wsReconnectionCount; i++ {
		if err = s.Connect(conn, requests...); err == nil {
			return nil
		}
		select {
		case <-time.After(s.wsReconnectionInterval):
			if err = s.Connect(conn, requests...); err != nil {
				continue
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return errors.New("Reconnection failed")
}

func (s *Stream) SetDebugMode(isDebugMode bool) {
	s.mu.Lock()
	s.isDebugMode = isDebugMode
	s.mu.Unlock()
}

func (s *Stream) SetReconnectionCount(count int) {
	s.mu.Lock()
	s.wsReconnectionCount = count
	s.mu.Unlock()
}

func (s *Stream) SetReconnectionInterval(interval time.Duration) {
	s.mu.Lock()
	s.wsReconnectionInterval = interval
	s.mu.Unlock()
}

func (s *Stream) Subscribe(conn *websocket.Conn, requests []models.WSRequest) (err error) {
	for _, req := range requests {
		if err = conn.WriteJSON(req); err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func (s *Stream) printf(format string, v ...interface{}) {
	if s.isDebugMode {
		log.Printf(fmt.Sprintf("%s%s", format, "\n"), v)
	}
}

func (s *Stream) Serve(
	ctx context.Context,
	conn *websocket.Conn,
	requests ...models.WSRequest) (chan interface{}, error) {

	for _, req := range requests {
		if req.ChannelType == models.FillsChannel || req.ChannelType == models.OrdersChannel {
			if err := s.Authorize(conn); err != nil {
				return nil, errors.WithStack(err)
			}
			break
		}
	}

	err := s.Connect(conn, requests...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	eventsC := make(chan interface{})
	msg := models.WsResponse{}

	go func() {

		go func() {
			for {
				s.client.mu.Lock()
				if err = s.GetEventResponse(
					ctx, conn, eventsC, &msg, requests...,
				); err != nil {
					s.client.mu.Unlock()
					return
				}
				s.client.mu.Unlock()
			}
		}()

		for {

			select {

			case <-ctx.Done():

				s.client.mu.Lock()
				err = conn.WriteMessage(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))

				if err != nil {
					s.printf("write close msg: %v", err)
					s.client.mu.Unlock()
					return
				}
				s.client.mu.Unlock()

				time.Sleep(time.Second)

				return

			case <-time.After(pingPeriod):

				s.printf("PING")

				s.client.mu.Lock()
				err = conn.WriteControl(
					websocket.PingMessage,
					[]byte(`{"op": "pong"}`),
					time.Now().UTC().Add(10*time.Second))

				if err != nil && err != websocket.ErrCloseSent {
					s.printf("write ping: %v", err)
				}
				s.client.mu.Unlock()

			}
		}
	}()

	return eventsC, err
}

func (s *Stream) SubscribeToTickers(
	ctx context.Context, symbols ...string) (wssub *WsSub, err error) {

	if len(symbols) == 0 {
		return nil, errors.New("symbols missing")
	}

	conn, err := s.CreateNewConnection()
	if err != nil {
		return nil, err
	}

	wssub = &WsSub{
		Conn:        conn,
		ChannelType: models.TickerChannel,
		Symbols:     symbols,
	}

	requests := wssub.MakeRequests()

	wssub.EventC, err = s.Serve(ctx, conn, requests...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	s.Subs = append(s.Subs, wssub)

	return
}

func (s *Stream) SubscribeToMarkets(
	ctx context.Context) (wssub *WsSub, err error) {

	conn, err := s.CreateNewConnection()
	if err != nil {
		return nil, err
	}

	wssub = &WsSub{
		Conn:        conn,
		ChannelType: models.MarketsChannel,
		Symbols:     []string{},
	}

	requests := wssub.MakeRequests()

	wssub.EventC, err = s.Serve(ctx, conn, requests...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	s.Subs = append(s.Subs)

	return
}

func (s *Stream) SubscribeToTrades(
	ctx context.Context, symbols ...string) (wssub *WsSub, err error) {

	if len(symbols) == 0 {
		return nil, errors.New("symbols missing")
	}

	conn, err := s.CreateNewConnection()
	if err != nil {
		return nil, err
	}

	wssub = &WsSub{
		Conn:        conn,
		ChannelType: models.TradesChannel,
		Symbols:     symbols,
	}

	requests := wssub.MakeRequests()

	wssub.EventC, err = s.Serve(ctx, conn, requests...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	s.Subs = append(s.Subs, wssub)

	return
}

func (s *Stream) SubscribeToOrderBooks(
	ctx context.Context, symbols ...string,
) (wssub *WsSub, err error) {

	if len(symbols) == 0 {
		return nil, errors.New("symbols is missing")
	}

	conn, err := s.CreateNewConnection()
	if err != nil {
		return nil, err
	}

	wssub = &WsSub{
		Conn:        conn,
		ChannelType: models.OrderBookChannel,
		Symbols:     symbols,
	}

	requests := wssub.MakeRequests()

	wssub.EventC, err = s.Serve(ctx, conn, requests...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	s.Subs = append(s.Subs, wssub)

	return
}

// TODO: Get fill and order streams to actually work right

func (s *Stream) SubscribeToFills(ctx context.Context) (wssub *WsSub, err error) {

	conn, err := s.CreateNewConnection()
	if err != nil {
		return nil, err
	}

	wssub = &WsSub{
		Conn:        conn,
		ChannelType: models.FillsChannel,
		Symbols:     []string{},
	}

	requests := wssub.MakeRequests()

	wssub.EventC, err = s.Serve(ctx, conn, requests...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	s.Subs = append(s.Subs, wssub)

	return
}

func (s *Stream) SubscribeToOrders(
	ctx context.Context, symbols ...string) (wssub *WsSub, err error) {

	if len(symbols) == 0 {
		return nil, errors.New("symbols missing")
	}

	conn, err := s.CreateNewConnection()
	if err != nil {
		return nil, err
	}

	wssub = &WsSub{
		Conn:        conn,
		ChannelType: models.OrderBookChannel,
		Symbols:     symbols,
	}

	requests := wssub.MakeRequests()

	wssub.EventC, err = s.Serve(ctx, conn, requests...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	s.Subs = append(s.Subs, wssub)

	return
}

func (ws *WsSub) MakeRequests() []models.WSRequest {
	return MakeRequests(ws.ChannelType, ws.Symbols...)
}

func (ws *WsSub) Subscribe() (err error) {

	if ws.Conn == nil {
		return fmt.Errorf("Nil connection pointer")
	}

	requests := ws.MakeRequests()

	for _, r := range requests {
		if err = ws.Conn.WriteJSON(r); err != nil {
			return errors.WithStack(err)
		}
	}

	return
}

func MapToMarketData(event interface{}) (map[string]*models.Market, error) {

	data, ok := event.(json.RawMessage)
	if !ok {
		return nil, fmt.Errorf("Convert fail")
	}

	var markets struct {
		Data map[string]*models.Market `json:"data"`
	}

	if err := json.Unmarshal(data, &markets); err != nil {
		return nil, fmt.Errorf("Unmarshal markets: %+v", err)
	}

	return markets.Data, nil
}
