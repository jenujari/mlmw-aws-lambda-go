package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
	"github.com/jedib0t/go-pretty/table"
	"github.com/markcheno/go-quote"
	"github.com/markcheno/go-talib"
	"github.com/samber/lo"
)

type Ticker struct {
	Name       string
	TickerName string
	volume     uint64
}

type FullQuote struct {
	*quote.Quote
	// HaOpen  []float64 `json:"haOpen"`
	// HaClose []float64 `json:"haClose"`
	// HaHigh  []float64 `json:"haHigh"`
	// HaLow   []float64 `json:"haLow"`
	RsiObv []float64 `json:"rsiObv"`
}

type BuyTicker struct {
	TickerName string
	RsiObv     float64
	Close      float64
	Volume     float64
}

type DataFrame []BuyTicker

type StrengthSorter []BuyTicker

func (a StrengthSorter) Len() int           { return len(a) }
func (a StrengthSorter) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a StrengthSorter) Less(i, j int) bool { return a[i].RsiObv > a[j].RsiObv }

type VolumeSorter []*Ticker

func (a VolumeSorter) Len() int           { return len(a) }
func (a VolumeSorter) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a VolumeSorter) Less(i, j int) bool { return a[i].volume > a[j].volume }

var allQuotesWG sync.WaitGroup
var quotesWG sync.WaitGroup
var quoteWG sync.WaitGroup
var dfMutex sync.Mutex
var df DataFrame = DataFrame{}

func main() {
	if _, exists := os.LookupEnv("AWS_LAMBDA_RUNTIME_API"); exists {
		lambda.Start(HandleLambdaEvent)
	} else {
		err := HandleLambdaEvent(context.Background(), nil)
		if err != nil {
			log.Fatal(err)
		}
	}
}

func HandleLambdaEvent(_ context.Context, _ json.RawMessage) error {
	clipStrChan := fetchClipBoardString()
	tickerList := formatStockList(clipStrChan)
	tickers := <-tickerList

	fmt.Println("Chartink gave " + strconv.Itoa(len(tickers)) + " no of symbols to check.")
	sort.Sort(VolumeSorter(tickers))

	if len(tickers) > 400 {
		tickers = tickers[0:400]
	}

	onlySymbols := lo.Map(tickers, func(s *Ticker, i int) string {
		return s.TickerName + "." + "NS"
	})

	allQuotesWG.Add(1)
	qtsCh := fetchQuotes(lo.Chunk(onlySymbols, CHUNKS))
	go processQuotesChan(qtsCh)

	allQuotesWG.Wait()
	quoteWG.Wait()

	sort.Sort(StrengthSorter(df))

	var message strings.Builder
	message.WriteString("<b>Small cap filter for " + time.Now().Format(DATE_FORMAT) + "</b>")
	message.WriteString("<pre>\n")
	message.WriteString(generateTeleGramText())
	message.WriteString("</pre>")
	hitTelegramURL(TCHANNEL_ID, message.String())
	return nil
	// writeToCSV(&df)
}

func generateTeleGramText() string {
	var tblstr strings.Builder

	t := table.NewWriter()
	t.SetOutputMirror(&tblstr)

	t.AppendHeader(table.Row{"Symbol", "Close"})
	for _, d := range df {
		t.AppendRow([]interface{}{d.TickerName, d.Close})
	}
	t.Render()

	return tblstr.String()
}

func hitTelegramURL(id string, message string) {
	var sb strings.Builder
	sb.WriteString("https://api.telegram.org/bot")
	sb.WriteString(BOT_ID)
	sb.WriteString("/sendMessage?chat_id=")
	sb.WriteString(id)
	sb.WriteString("&parse_mode=HTML")
	sb.WriteString("&text=")
	sb.WriteString(url.QueryEscape(message))
	_, err := http.Get(sb.String())
	if err != nil {
		fmt.Println("Error in hitting the telegram url")
		fmt.Println(err)
	}
}

func fetchQuotes(chunks [][]string) <-chan quote.Quotes {
	today := time.Now().AddDate(0, 0, 1)
	twoYrBack := today.AddDate(-2, 0, 0)
	quotesChan := make(chan quote.Quotes)

	go func() {
		for itr, symblList := range chunks {
			quotesWG.Add(1)
			go func(i int, list []string) {
				fmt.Printf("\n%d no of itreation for featching HLOC data for %d items from yahoo finance api.", i, CHUNKS)

				qts, e := quote.NewQuotesFromYahooSyms(list, twoYrBack.Format(DATE_FORMAT), today.Format(DATE_FORMAT), quote.Daily, false)

				if e != nil {
					fmt.Println(e)
					quotesWG.Done()
					return
				}

				quotesChan <- qts
			}(itr, symblList)
		}
		quotesWG.Wait()
		<-time.After(time.Second)
		close(quotesChan)
		allQuotesWG.Done()
	}()

	return quotesChan
}

func processQuotesChan(ch <-chan quote.Quotes) {
	for qts := range ch {
		for _, quote := range qts {
			upto := len(quote.Date)

			if upto < 90 {
				continue
			}

			quoteWG.Add(1)
			go processQuoteAndUpdateDf(quote, upto)
		}
		quotesWG.Done()
	}
}

func processQuoteAndUpdateDf(q quote.Quote, upto int) {
	// hHighs, hOpens, hCloses, hLows := talib.HeikinashiCandles(quote.High, quote.Open, quote.Close, quote.Low)
	obv := talib.Obv(q.Close, q.Volume)
	rsiObv := talib.Rsi(obv, 14)

	fqts := QuoteToFullQuote(&q)

	for i := range fqts.Date {
		fqts.Close[i] = TruncateFloat(fqts.Close[i])
		fqts.Open[i] = TruncateFloat(fqts.Open[i])
		fqts.High[i] = TruncateFloat(fqts.High[i])
		fqts.Low[i] = TruncateFloat(fqts.Low[i])
		// fqts.HaHigh[i] = TruncateFloat(hHighs[i])
		// fqts.HaClose[i] = TruncateFloat(hCloses[i])
		// fqts.HaOpen[i] = TruncateFloat(hOpens[i])
		// fqts.HaLow[i] = TruncateFloat(hLows[i])
		fqts.Volume[i] = TruncateFloat(fqts.Volume[i])
		fqts.RsiObv[i] = TruncateFloat(rsiObv[i])
	}

	if buyCondition(fqts, upto-1) {
		bt := BuyTicker{
			TickerName: fqts.Symbol,
			Close:      fqts.Close[upto-1],
			RsiObv:     fqts.RsiObv[upto-1],
			Volume:     fqts.Volume[upto-1],
		}
		dfMutex.Lock()
		df = append(df, bt)
		dfMutex.Unlock()
	}

	fmt.Printf("\nCompleted calculating obv rsi for %s.", q.Symbol)
	quoteWG.Done()
}

// func writeToCSV(df *DataFrame) {
// 	var sb strings.Builder

// 	sb.WriteString(time.Now().Format(DATE_FORMAT))
// 	sb.WriteString(".csv")

// 	file, e := os.Create(sb.String())
// 	if e != nil {
// 		fmt.Printf("Error happend while creating the csv file for %s \n", time.Now().Format(DATE_FORMAT))
// 		fmt.Println(e)
// 		return
// 	}

// 	csvW := csv.NewWriter(file)
// 	csvW.Write([]string{"Symbol", "Close", "ObvRsi"})

// 	for _, dr := range *df {
// 		entry := make([]string, 3)
// 		entry[0] = dr.TickerName
// 		entry[1] = fmt.Sprintf("%.2f", dr.Close)
// 		entry[2] = fmt.Sprintf("%.2f", dr.RsiObv)

// 		csvW.Write(entry)
// 	}
// 	csvW.Flush()
// 	file.Close()
// }

func buyCondition(q *FullQuote, d int) bool {
	return q.RsiObv[d] >= 62
}

func TruncateFloat(x float64) float64 {
	return math.Round(x*100) / 100
}

func QuoteToFullQuote(q *quote.Quote) *FullQuote {
	l := len(q.Date)
	qf := new(FullQuote)

	qf.Quote = q
	// qf.HaOpen = make([]float64, l)
	// qf.HaLow = make([]float64, l)
	// qf.HaClose = make([]float64, l)
	// qf.HaHigh = make([]float64, l)
	qf.RsiObv = make([]float64, l)
	return qf
}

func formatStockList(clipStr <-chan string) <-chan []*Ticker {
	tickerChan := make(chan []*Ticker)
	go func() {
		clipstring := <-clipStr
		lines := strings.Split(clipstring, "\n")
		headers := strings.Split(strings.ReplaceAll(lines[2], "\r", ""), "\t")
		chunks := lines[3:]
		var tickerList []*Ticker
		for _, chRow := range chunks {
			ticker := &Ticker{}
			rwCols := strings.Split(chRow, "\t")
			for i, head := range headers {
				if head == "Stock Name" {
					ticker.Name = rwCols[i]
				}
				if head == "Symbol" {
					ticker.TickerName = rwCols[i]
				}
				if head == "Volume" {
					ticker.volume, _ = strconv.ParseUint(strings.ReplaceAll(rwCols[i], ",", ""), 10, 64)
				}
			}
			tickerList = append(tickerList, ticker)
		}
		tickerChan <- tickerList
		close(tickerChan)
	}()
	return tickerChan
}

func fetchClipBoardString() <-chan string {
	clipStr := make(chan string)
	go func() {
		opts := []chromedp.ExecAllocatorOption{
			chromedp.DisableGPU,
			chromedp.NoSandbox,
			chromedp.Headless,
			chromedp.Flag("no-zygote", true),
			chromedp.Flag("single-process", true),
			chromedp.Flag("homedir", "/tmp"),
			chromedp.Flag("data-path", "/tmp/data-path"),
			chromedp.Flag("disk-cache-dir", "/tmp/cache-dir"),
			chromedp.Flag("remote-debugging-port", "9222"),
			chromedp.Flag("remote-debugging-address", "0.0.0.0"),
			chromedp.Flag("disable-dev-shm-usage", true),
			chromedp.Flag("disable-setuid-sandbox", true),
		}

		allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
		defer cancel()

		ctx, cancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
		defer cancel()

		var buf string

		if err := chromedp.Run(ctx, captureStocks(URLTOVISIT, &buf)); err != nil {
			log.Fatal(err)
		}

		clipStr <- buf
		close(clipStr)
	}()
	return clipStr
}

func captureStocks(urlstr string, res *string) chromedp.Tasks {
	d := browser.PermissionDescriptor{Name: "clipboard-read", AllowWithoutSanitization: true}
	return chromedp.Tasks{
		browser.SetPermission(&d, browser.PermissionSettingGranted).WithOrigin("https://chartink.com"),
		chromedp.Navigate(urlstr),
		chromedp.WaitVisible(`#DataTables_Table_0`),
		chromedp.Click(`button.btn.buttons-copy`, chromedp.NodeVisible),
		chromedp.Evaluate(getEvalFunc(), nil),
		chromedp.WaitVisible(`#mycliptext`),
		chromedp.Text(`#mycliptext`, res, chromedp.NodeVisible),
	}
}

func getEvalFunc() string {
	return `navigator.clipboard.readText().then((clipText) => {
		let dvx = document.getElementById('DataTables_Table_0_wrapper');
		let dv =  document.createElement('pre');
		dv.setAttribute('id','mycliptext');
		dv.innerText = clipText;
		dvx.appendChild(dv)
	});`
}

/*
type MyEvent struct {
	Name string `json:"What is your name?"`
	Age int     `json:"How old are you?"`
}

type MyResponse struct {
	Message string `json:"Answer:"`
}

func HandleLambdaEvent(event MyEvent) (MyResponse, error) {
	return MyResponse{Message: fmt.Sprintf("%s is %d years old!", event.Name, event.Age)}, nil
}
*/
