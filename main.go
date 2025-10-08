package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Depth struct {
	Bids map[float64]float64
	Asks map[float64]float64
	sync.RWMutex
}

func NewDepth() *Depth {
	return &Depth{
		Bids: make(map[float64]float64),
		Asks: make(map[float64]float64),
	}
}

func (d *Depth) Update(side string, price, qty float64) {
	d.Lock()
	defer d.Unlock()
	book := d.Bids
	if side == "Sell" {
		book = d.Asks
	}
	if qty == 0 {
		delete(book, price)
	} else {
		book[price] = qty
	}
}

func (d *Depth) Snapshot(bids, asks [][]interface{}) {
	d.Lock()
	defer d.Unlock()
	d.Bids = make(map[float64]float64)
	d.Asks = make(map[float64]float64)
	for _, bid := range bids {
		price, _ := bid[0].(string)
		qty, _ := bid[1].(string)
		var p, q float64
		fmt.Sscanf(price, "%f", &p)
		fmt.Sscanf(qty, "%f", &q)
		d.Bids[p] = q
	}
	for _, ask := range asks {
		price, _ := ask[0].(string)
		qty, _ := ask[1].(string)
		var p, q float64
		fmt.Sscanf(price, "%f", &p)
		fmt.Sscanf(qty, "%f", &q)
		d.Asks[p] = q
	}
}

type Trade struct {
	Side  string  `json:"side"`
	Price float64 `json:"price"`
	Qty   float64 `json:"qty"`
	Time  int64   `json:"time"`
}

type Trades struct {
	List []Trade
	sync.RWMutex
	Max int
}

func NewTrades(max int) *Trades {
	return &Trades{Max: max}
}

func (t *Trades) Add(trade Trade) {
	t.Lock()
	defer t.Unlock()
	t.List = append([]Trade{trade}, t.List...)
	if len(t.List) > t.Max {
		t.List = t.List[:t.Max]
	}
}

func (t *Trades) Get() []Trade {
	t.RLock()
	defer t.RUnlock()
	return append([]Trade(nil), t.List...)
}

type TradeVolumeBuckets struct {
	BidVol map[float64]float64
	AskVol map[float64]float64
	sync.RWMutex
}

func NewTradeVolumeBuckets() *TradeVolumeBuckets {
	return &TradeVolumeBuckets{
		BidVol: make(map[float64]float64),
		AskVol: make(map[float64]float64),
	}
}

func (tvb *TradeVolumeBuckets) Add(side string, price, qty, bucketSize float64) {
	tvb.Lock()
	defer tvb.Unlock()

	bucket := float64(int(price/bucketSize)) * bucketSize

	if side == "Buy" {
		tvb.AskVol[bucket] += qty
	} else {
		tvb.BidVol[bucket] += qty
	}
}

func (tvb *TradeVolumeBuckets) Get() (map[float64]float64, map[float64]float64) {
	tvb.RLock()
	defer tvb.RUnlock()

	bidVol := make(map[float64]float64)
	askVol := make(map[float64]float64)

	for k, v := range tvb.BidVol {
		bidVol[k] = v
	}
	for k, v := range tvb.AskVol {
		askVol[k] = v
	}

	return bidVol, askVol
}

type DOMRow struct {
	BidQty float64 `json:"bid_qty"`
	BidVol float64 `json:"bid_vol"`
	Price  float64 `json:"price"`
	AskQty float64 `json:"ask_qty"`
	AskVol float64 `json:"ask_vol"`
	IsMid  bool    `json:"is_mid"`
}

type DOMOrderBook struct {
	Rows         []DOMRow `json:"rows"`
	CurrentPrice float64  `json:"current_price"`
	Trades       []Trade  `json:"trades"`
}

func (d *Depth) GetDOMRows(tvb *TradeVolumeBuckets, currentPrice float64, bucketSize float64) []DOMRow {
	d.RLock()
	defer d.RUnlock()

	bidBuckets := make(map[float64]float64)
	askBuckets := make(map[float64]float64)
	priceMap := make(map[float64]struct{})

	for price, qty := range d.Bids {
		bucket := float64(int(price/bucketSize)) * bucketSize
		bidBuckets[bucket] += qty
		priceMap[bucket] = struct{}{}
	}
	for price, qty := range d.Asks {
		bucket := float64(int(price/bucketSize)) * bucketSize
		askBuckets[bucket] += qty
		priceMap[bucket] = struct{}{}
	}

	bidVol, askVol := tvb.Get()
	for price := range bidVol {
		priceMap[price] = struct{}{}
	}
	for price := range askVol {
		priceMap[price] = struct{}{}
	}

	currentBucket := float64(int(currentPrice/bucketSize)) * bucketSize
	rangeSize := 13

	for i := -rangeSize; i <= rangeSize; i++ {
		bucket := currentBucket + float64(i)*bucketSize
		priceMap[bucket] = struct{}{}
	}

	var prices []float64
	for p := range priceMap {
		prices = append(prices, p)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(prices)))

	rows := []DOMRow{}
	midBucket := currentBucket

	for _, price := range prices {
		row := DOMRow{
			BidQty: bidBuckets[price],
			BidVol: bidVol[price],
			Price:  price,
			AskQty: askBuckets[price],
			AskVol: askVol[price],
			IsMid:  (price == midBucket),
		}
		rows = append(rows, row)
	}
	return rows
}

type Hub struct {
	clients    map[*websocket.Conn]bool
	broadcast  chan DOMOrderBook
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	sync.RWMutex
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan DOMOrderBook, 10),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case conn := <-h.register:
			h.Lock()
			h.clients[conn] = true
			h.Unlock()
		case conn := <-h.unregister:
			h.Lock()
			if _, ok := h.clients[conn]; ok {
				delete(h.clients, conn)
				conn.Close()
			}
			h.Unlock()
		case book := <-h.broadcast:
			h.RLock()
			for conn := range h.clients {
				conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
				err := conn.WriteJSON(book)
				if err != nil {
					conn.Close()
					delete(h.clients, conn)
				}
			}
			h.RUnlock()
		}
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsHandler(hub *Hub, depth *Depth, trades *Trades, tvb *TradeVolumeBuckets, bucketSize float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		hub.register <- conn

		trs := trades.Get()
		currentPrice := 0.0
		if len(trs) > 0 {
			currentPrice = trs[0].Price
		}
		rows := depth.GetDOMRows(tvb, currentPrice, bucketSize)
		conn.WriteJSON(DOMOrderBook{
			Rows:         rows,
			CurrentPrice: currentPrice,
			Trades:       trs,
		})

		go func() {
			defer func() { hub.unregister <- conn }()
			for {
				if _, _, err := conn.NextReader(); err != nil {
					break
				}
			}
		}()
	}
}

func main() {
	url := "wss://stream.bybit.com/v5/public/linear"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		log.Fatal("dial:", err)
	}
	defer c.Close()

	sub := map[string]interface{}{
		"op":   "subscribe",
		"args": []string{"orderbook.500.BTCUSDT", "publicTrade.BTCUSDT"},
	}
	if err := c.WriteJSON(sub); err != nil {
		log.Fatal("subscribe:", err)
	}

	depth := NewDepth()
	trades := NewTrades(20)
	tvb := NewTradeVolumeBuckets()
	hub := NewHub()
	go hub.Run()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, htmlPage)
	})
	http.HandleFunc("/ws", wsHandler(hub, depth, trades, tvb, 10))

	go func() {
		log.Println("HTTP server at http://localhost:8080")
		log.Fatal(http.ListenAndServe(":8080", nil))
	}()

	go func() {
		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				log.Println("read:", err)
				return
			}
			var resp map[string]interface{}
			if err := json.Unmarshal(message, &resp); err != nil {
				continue
			}
			topic, _ := resp["topic"].(string)
			switch {
			case topic == "orderbook.500.BTCUSDT":
				typ, _ := resp["type"].(string)
				data, _ := resp["data"].(map[string]interface{})
				if typ == "snapshot" {
					bids, _ := data["b"].([]interface{})
					asks, _ := data["a"].([]interface{})
					var bidList, askList [][]interface{}
					for _, b := range bids {
						bidList = append(bidList, b.([]interface{}))
					}
					for _, a := range asks {
						askList = append(askList, a.([]interface{}))
					}
					depth.Snapshot(bidList, askList)
				} else if typ == "delta" {
					if b, ok := data["b"].([]interface{}); ok {
						for _, bid := range b {
							arr := bid.([]interface{})
							price, _ := arr[0].(string)
							qty, _ := arr[1].(string)
							var p, q float64
							fmt.Sscanf(price, "%f", &p)
							fmt.Sscanf(qty, "%f", &q)
							depth.Update("Buy", p, q)
						}
					}
					if a, ok := data["a"].([]interface{}); ok {
						for _, ask := range a {
							arr := ask.([]interface{})
							price, _ := arr[0].(string)
							qty, _ := arr[1].(string)
							var p, q float64
							fmt.Sscanf(price, "%f", &p)
							fmt.Sscanf(qty, "%f", &q)
							depth.Update("Sell", p, q)
						}
					}
				}

				trs := trades.Get()
				currentPrice := 0.0
				if len(trs) > 0 {
					currentPrice = trs[0].Price
				}

				rows := depth.GetDOMRows(tvb, currentPrice, 10)
				hub.broadcast <- DOMOrderBook{
					Rows:         rows,
					CurrentPrice: currentPrice,
					Trades:       trs,
				}

			case topic == "publicTrade.BTCUSDT":
				data, _ := resp["data"].([]interface{})
				for _, t := range data {
					trade := t.(map[string]interface{})
					side, _ := trade["S"].(string)
					priceStr, _ := trade["p"].(string)
					qtyStr, _ := trade["v"].(string)
					ts, _ := trade["T"].(float64)
					var price, qty float64
					fmt.Sscanf(priceStr, "%f", &price)
					fmt.Sscanf(qtyStr, "%f", &qty)
					trades.Add(Trade{
						Side:  side,
						Price: price,
						Qty:   qty,
						Time:  int64(ts),
					})
					tvb.Add(side, price, qty, 10)
				}

				trs := trades.Get()
				currentPrice := 0.0
				if len(trs) > 0 {
					currentPrice = trs[0].Price
				}
				rows := depth.GetDOMRows(tvb, currentPrice, 10)
				hub.broadcast <- DOMOrderBook{
					Rows:         rows,
					CurrentPrice: currentPrice,
					Trades:       trs,
				}
			}
		}
	}()

	<-interrupt
	fmt.Println("Interrupted, closing connection.")
}

const htmlPage = `
<!DOCTYPE html>
<html>
<head>
    <title>Order Book with Persistent Volume Tracking</title>
    <style>
        body { 
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif; 
            background: #1a1a1a;
            color: #ffffff;
            margin: 0;
            padding: 20px;
            transition: background 0.3s ease;
            overflow-x: hidden;
        }
        
        /* Fullscreen styles */
        body.fullscreen {
            padding: 10px;
        }
        
        .fullscreen-toggle {
            position: fixed;
            top: 10px;
            right: 10px;
            background: #333;
            color: #fff;
            border: none;
            padding: 8px 12px;
            border-radius: 4px;
            cursor: pointer;
            z-index: 1000;
            font-size: 12px;
            transition: background 0.3s ease;
        }
        
        .fullscreen-toggle:hover {
            background: #555;
        }
        
        .container { 
            display: flex; 
            flex-direction: row; 
            align-items: flex-start; 
            gap: 16px;
            height: calc(100vh - 40px);
        }
        
        body.fullscreen .container {
            height: calc(100vh - 20px);
        }
        
        .orderbook-panel { 
            flex: 3; 
            background: #2a2a2a;
            border-radius: 8px;
            padding: 15px;
            overflow: hidden;
            display: flex;
            flex-direction: column;
        }
        
        .trades-panel { 
            flex: 1; 
            background: #2a2a2a;
            border-radius: 8px;
            padding: 15px;
            overflow: hidden;
            display: flex;
            flex-direction: column;
        }
        
        table { 
            border-collapse: collapse; 
            margin: 10px 0; 
            width: 100%;
            background: #333;
        }
        
        #dom-table {
            flex: 1;
            overflow-y: auto;
            display: block;
        }
        
        #dom-table thead,
        #dom-table tbody {
            display: table;
            width: 100%;
            table-layout: fixed;
        }
        
        #trades {
            flex: 1;
            overflow-y: auto;
            display: block;
        }
        
        #trades thead,
        #trades tbody {
            display: table;
            width: 100%;
            table-layout: fixed;
        }
        
        th, td { 
            border: 1px solid #555; 
            padding: 2px 4px; 
            text-align: right; 
            font-size: 11px;
        }
        
        th { 
            background: #444; 
            color: #fff;
            font-weight: bold;
            position: sticky;
            top: 0;
            z-index: 10;
        }
        
        .bid { 
            background: rgba(76, 175, 80, 0.1); 
            color: #4caf50;
        }
        
        .ask { 
            background: rgba(244, 67, 54, 0.1); 
            color: #f44336;
        }
        
        .mid { 
            background: #ffc107 !important; 
            color: #000 !important;
            font-weight: bold; 
        }
        
        .trade-buy { 
            background: rgba(76, 175, 80, 0.2); 
            color: #4caf50; 
        }
        
        .trade-sell { 
            background: rgba(244, 67, 54, 0.2); 
            color: #f44336; 
        }
        
        #current-price { 
            font-size: 1.8em; 
            font-weight: bold; 
            margin-bottom: 10px; 
            text-align: center;
            padding: 10px;
            border-radius: 8px;
            background: #333;
            position: sticky;
            top: 0;
            z-index: 100;
        }
        
        .trades-panel h3, .orderbook-panel h3 { 
            margin-top: 0; 
            color: #fff;
            text-align: center;
            margin-bottom: 10px;
        }
        
        /* Custom volume column styling */
        .bid-vol-cell {
            background: #000000 !important;
            color: #ff4444 !important;
        }
        
        .ask-vol-cell {
            background: #000000 !important;
            color: #00BD31 !important;
        }
        
        /* Volume intensity styling */
        .volume-low { opacity: 0.7; }
        .volume-medium { opacity: 0.85; }
        .volume-high { opacity: 1.0; font-weight: bold; }
        
        /* Scrollbar styling */
        ::-webkit-scrollbar {
            width: 8px;
        }
        ::-webkit-scrollbar-track {
            background: #333;
        }
        ::-webkit-scrollbar-thumb {
            background: #666;
            border-radius: 4px;
        }
        ::-webkit-scrollbar-thumb:hover {
            background: #888;
        }
        
        /* Hide scrollbars in fullscreen mode */
        body.fullscreen ::-webkit-scrollbar {
            width: 4px;
        }
        
        /* Status indicator */
        .connection-status {
            position: fixed;
            top: 10px;
            left: 10px;
            background: #333;
            color: #fff;
            padding: 4px 8px;
            border-radius: 4px;
            font-size: 10px;
            z-index: 1000;
        }
        
        .connection-status.connected {
            background: #4caf50;
        }
        
        .connection-status.disconnected {
            background: #f44336;
        }
    </style>
</head>
<body>
    <button class="fullscreen-toggle" onclick="toggleFullscreen()">⛶ Fullscreen</button>
    <div class="connection-status" id="connection-status">Connecting...</div>
    
    <div class="container">
        <div class="trades-panel">
            <h3>Recent Trades</h3>
            <table id="trades">
                <thead>
                    <tr>
                        <th>Price</th><th>Quantity</th>
                    </tr>
                </thead>
                <tbody id="trades-body"></tbody>
            </table>
        </div>
        <div class="orderbook-panel">
            <div id="current-price"></div>
            <table id="dom-table">
                <thead>
                    <tr>
                        <th>Bid Qty</th>
                        <th>Bid Vol</th>
                        <th>Price</th>
                        <th>Ask Vol</th>
                        <th>Ask Qty</th>
                    </tr>
                </thead>
                <tbody id="dom-body"></tbody>
            </table>
        </div>
    </div>
    
    <script>
        let lastPrice = null;
        let isFullscreen = false;
        
        function toggleFullscreen() {
            if (!isFullscreen) {
                // Enter fullscreen
                if (document.documentElement.requestFullscreen) {
                    document.documentElement.requestFullscreen();
                } else if (document.documentElement.webkitRequestFullscreen) {
                    document.documentElement.webkitRequestFullscreen();
                } else if (document.documentElement.mozRequestFullScreen) {
                    document.documentElement.mozRequestFullScreen();
                } else if (document.documentElement.msRequestFullscreen) {
                    document.documentElement.msRequestFullscreen();
                }
            } else {
                // Exit fullscreen
                if (document.exitFullscreen) {
                    document.exitFullscreen();
                } else if (document.webkitExitFullscreen) {
                    document.webkitExitFullscreen();
                } else if (document.mozCancelFullScreen) {
                    document.mozCancelFullScreen();
                } else if (document.msExitFullscreen) {
                    document.msExitFullscreen();
                }
            }
        }
        
        // Listen for fullscreen changes
        document.addEventListener('fullscreenchange', handleFullscreenChange);
        document.addEventListener('webkitfullscreenchange', handleFullscreenChange);
        document.addEventListener('mozfullscreenchange', handleFullscreenChange);
        document.addEventListener('MSFullscreenChange', handleFullscreenChange);
        
        function handleFullscreenChange() {
            isFullscreen = !!(document.fullscreenElement || 
                             document.webkitFullscreenElement || 
                             document.mozFullScreenElement || 
                             document.msFullscreenElement);
            
            document.body.classList.toggle('fullscreen', isFullscreen);
            
            const button = document.querySelector('.fullscreen-toggle');
            button.textContent = isFullscreen ? '⛶ Exit Fullscreen' : '⛶ Fullscreen';
        }
        
        // Keyboard shortcut for fullscreen (F11 or F)
        document.addEventListener('keydown', function(e) {
            if (e.key === 'F11' || (e.key === 'f' || e.key === 'F')) {
                e.preventDefault();
                toggleFullscreen();
            }
            // ESC to exit fullscreen
            if (e.key === 'Escape' && isFullscreen) {
                toggleFullscreen();
            }
        });
        
        function getVolumeClass(volume, maxVolume) {
            if (!volume || volume === 0) return '';
            const ratio = volume / maxVolume;
            if (ratio > 0.7) return 'volume-high';
            if (ratio > 0.3) return 'volume-medium';
            return 'volume-low';
        }
        
        let lastBucket = null;
        
        function animatePrice(price) {
            const el = document.getElementById("current-price");
            el.textContent = "Current Price: $" + price.toFixed(2);
            el.style.transition = "background 3s";
            if (lastPrice !== null) {
                if (price > lastPrice) {
                    el.style.background = "#4caf50";
                } else if (price < lastPrice) {
                    el.style.background = "#f44336";
                } else {
                    el.style.background = "#333";
                }
            }
            lastPrice = price;
            setTimeout(() => { el.style.background = "#333"; }, 800);
        }
        
        function animateBucketJump(currentBucket) {
            if (lastBucket !== null && lastBucket !== currentBucket) {
                // Animate the whole background
                if (currentBucket > lastBucket) {
                    // Price moved up to higher bucket - green background
                    document.body.style.background = "#2d5016";
                } else {
                    // Price moved down to lower bucket - red background  
                    document.body.style.background = "#5d1f1f";
                }
                setTimeout(() => { 
                    document.body.style.background = "#1a1a1a"; 
                }, 800);
            }
            lastBucket = currentBucket;
        }
        
        function updateConnectionStatus(status) {
            const el = document.getElementById('connection-status');
            el.className = 'connection-status ' + status;
            el.textContent = status === 'connected' ? 'Connected' : 
                           status === 'disconnected' ? 'Disconnected' : 'Connecting...';
        }
        
        let ws = new WebSocket("ws://" + location.host + "/ws");
        
        ws.onopen = function() {
            updateConnectionStatus('connected');
        };
        
        ws.onmessage = function(event) {
            let data = JSON.parse(event.data);
            let rows = data.rows, price = data.current_price, trades = data.trades || [];
            
            animatePrice(price);
            
            // Calculate current bucket for bucket jump animation
            const bucketSize = 10; // Should match the Go code bucket size
            const currentBucket = Math.floor(price / bucketSize) * bucketSize;
            
            // Find max volume for scaling
            let maxBidVol = 0, maxAskVol = 0;
            for (let row of rows) {
                if (row.bid_vol > maxBidVol) maxBidVol = row.bid_vol;
                if (row.ask_vol > maxAskVol) maxAskVol = row.ask_vol;
            }
            
            let html = "";
            for (let i = 0; i < rows.length; i++) {
                let row = rows[i];
                let midClass = row.is_mid ? "mid" : "";
                let bidVolClass = getVolumeClass(row.bid_vol, maxBidVol);
                let askVolClass = getVolumeClass(row.ask_vol, maxAskVol);
                
                html += "<tr class='" + midClass + "'>";
                html += "<td class='bid " + (row.bid_qty ? "volume-medium" : "") + "'>" + 
                        (row.bid_qty ? row.bid_qty.toFixed(4) : "") + "</td>";
                html += "<td class='bid-vol-cell " + bidVolClass + "'>" + 
                        (row.bid_vol ? row.bid_vol.toFixed(4) : "") + "</td>";
                html += "<td style='color: #fff; background: #444;'>" + row.price.toFixed(2) + "</td>";
                html += "<td class='ask-vol-cell " + askVolClass + "'>" + 
                        (row.ask_vol ? row.ask_vol.toFixed(4) : "") + "</td>";
                html += "<td class='ask " + (row.ask_qty ? "volume-medium" : "") + "'>" + 
                        (row.ask_qty ? row.ask_qty.toFixed(4) : "") + "</td>";
                html += "</tr>";
            }
            document.getElementById("dom-body").innerHTML = html;
            
            // Animate bucket jump after DOM is updated
            animateBucketJump(currentBucket);

            // Update trades table
            let tradesHtml = "";
            for (let t of trades) {
                let cls = t.side === "Buy" ? "trade-buy" : "trade-sell";
                tradesHtml += "<tr class='" + cls + "'>";
                tradesHtml += "<td>" + t.price.toFixed(2) + "</td>";
                tradesHtml += "<td>" + t.qty.toFixed(4) + "</td>";
                tradesHtml += "</tr>";
            }
            document.getElementById("trades-body").innerHTML = tradesHtml;
        };
        
        ws.onclose = function() {
            console.log("WebSocket connection closed, attempting to reconnect...");
            updateConnectionStatus('disconnected');
            setTimeout(() => {
                location.reload();
            }, 3000);
        };
        
        ws.onerror = function(error) {
            console.log("WebSocket error:", error);
            updateConnectionStatus('disconnected');
        };
        
        // Auto-enter fullscreen on load (optional - might be blocked by browser)
        document.addEventListener('DOMContentLoaded', function() {
            // Uncomment the next line if you want auto-fullscreen on load
            // setTimeout(() => toggleFullscreen(), 1000);
        });
    </script>
</body>
</html>
`
