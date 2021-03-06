package uploader

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/lomik/go-carbon/helper"
)

type Option func(u *Uploader)

func Path(path string) Option {
	return func(u *Uploader) {
		u.path = path
	}
}

func ClickHouse(dsn string) Option {
	return func(u *Uploader) {
		u.clickHouseDSN = dsn
	}
}

func DataTable(t string) Option {
	return func(u *Uploader) {
		u.dataTable = t
	}
}

func DataTimeout(t time.Duration) Option {
	return func(u *Uploader) {
		u.dataTimeout = t
	}
}

func TreeTable(t string) Option {
	return func(u *Uploader) {
		u.treeTable = t
	}
}

func TreeTimeout(t time.Duration) Option {
	return func(u *Uploader) {
		u.treeTimeout = t
	}
}

func InProgressCallback(cb func(string) bool) Option {
	return func(u *Uploader) {
		u.inProgressCallback = cb
	}
}

func Threads(t int) Option {
	return func(u *Uploader) {
		u.threads = t
	}
}

// Uploader upload files from local directory to clickhouse
type Uploader struct {
	helper.Stoppable
	sync.Mutex
	path               string
	clickHouseDSN      string
	dataTable          string
	dataTimeout        time.Duration
	treeTable          string
	treeTimeout        time.Duration
	filesUploaded      uint64 // stat "files"
	threads            int
	inProgressCallback func(string) bool
	queue              chan string
	inQueue            map[string]bool // current uploading files
	treeExists         CMap            // store known keys and don't load it to clickhouse tree
}

func New(options ...Option) *Uploader {

	u := &Uploader{
		path:               "/data/carbon-clickhouse/",
		dataTable:          "graphite",
		treeTable:          "graphite_tree",
		dataTimeout:        time.Minute,
		treeTimeout:        time.Minute,
		inProgressCallback: func(string) bool { return false },
		queue:              make(chan string, 1024),
		inQueue:            make(map[string]bool),
		threads:            1,
		treeExists:         NewCMap(),
	}

	for _, o := range options {
		o(u)
	}

	return u
}

func (u *Uploader) Start() error {
	return u.StartFunc(func() error {
		u.Go(u.watchWorker)

		for i := 0; i < u.threads; i++ {
			u.Go(u.uploadWorker)
		}

		return nil
	})
}

func uploadData(chUrl string, table string, timeout time.Duration, data io.Reader) error {
	p, err := url.Parse(chUrl)
	if err != nil {
		return err
	}

	q := p.Query()

	q.Set("query", fmt.Sprintf("INSERT INTO %s FORMAT TabSeparated", table))

	p.RawQuery = q.Encode()
	queryUrl := p.String()

	req, err := http.NewRequest("POST", queryUrl, data)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return fmt.Errorf("clickhouse response status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (u *Uploader) upload(exit chan bool, filename string) (err error) {
	startTime := time.Now()
	logrus.Infof("[uploader] start handle %s", filename)

	defer func() {
		if err != nil {
			logrus.Errorf("[uploader] %s", err.Error())
		} else {
			logrus.Infof("[uploader] handle %s success, time=%s", filename, time.Now().Sub(startTime).String())
		}
	}()

	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return err
	}

	if fi.Size() == 0 {
		logrus.Infof("[uploader] %s is empty", filename)
		return nil
	}

	err = uploadData(u.clickHouseDSN, u.dataTable, u.dataTimeout, file)

	if err != nil {
		return err
	}

	if u.treeTable == "" { // don't make index in clickhouse
		return nil
	}

	// MAKE INDEX

	// reopen file
	file, err = os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	if err != nil {
		return err
	}

	reader := bufio.NewReaderSize(file, 1024*1024)

	treeData := bytes.NewBuffer(nil)

	localUniq := make(map[string]bool)

	var key string
	var level int
	var exists bool
	var date string

LineLoop:
	for {
		line, _, err := reader.ReadLine()
		if err == io.EOF {
			break
		}

		if err != nil {
			log.Fatal(err)
		}

		row := strings.Split(string(line), "\t")
		metric := row[0]

		if u.treeExists.Exists(metric) {
			continue LineLoop
		}

		if _, exists = localUniq[metric]; exists {
			continue LineLoop
		}

		date = row[3][:8] + "01" // first day of month

		offset := 0
		for level = 1; ; level++ {
			p := strings.IndexByte(metric[offset:], '.')
			if p < 0 {
				break
			}
			key = metric[:offset+p+1]

			if !u.treeExists.Exists(key) {
				if _, exists := localUniq[key]; !exists {
					localUniq[key] = true
					fmt.Fprintf(treeData, "%s\t%d\t%s\n", date, level, key)
				}
			}

			offset += p + 1
		}

		localUniq[metric] = true
		fmt.Fprintf(treeData, "%s\t%d\t%s\n", date, level, metric)
	}

	// @TODO: insert to tree data metrics
	err = uploadData(u.clickHouseDSN, u.treeTable, u.treeTimeout, treeData)
	if err != nil {
		return err
	}

	// copy data from localUniq to global
	for key, _ = range localUniq {
		u.treeExists.Add(key)
	}

	return nil
}

func (u *Uploader) uploadWorker(exit chan bool) {
	for {
		select {
		case <-exit:
			return
		case filename := <-u.queue:
			err := u.upload(exit, filename)
			if err == nil {
				err := os.Remove(filename)
				if err != nil {
					logrus.Errorf("[uploader] remove %s failed: %s", filename, err.Error())
				} else {
					logrus.Infof("[uploader] %s deleted", filename)
				}
			}
			u.Lock()
			delete(u.inQueue, filename)
			u.Unlock()
		}
	}
}

func (u *Uploader) watch(exit chan bool) {
	flist, err := ioutil.ReadDir(u.path)
	if err != nil {
		logrus.Errorf("[uploader] %s", err.Error())
		return
	}

	files := make([]string, 0)
	for _, f := range flist {
		if f.IsDir() {
			continue
		}
		if !strings.HasPrefix(f.Name(), "default.") {
			continue
		}

		files = append(files, path.Join(u.path, f.Name()))
	}

	if len(files) == 0 {
		return
	}

	sort.Strings(files)

	for _, fn := range files {
		if u.inProgressCallback(fn) { // write in progress
			continue
		}

		u.Lock()
		if u.inQueue[fn] {
			u.Unlock()
			continue
		} else {
			u.inQueue[fn] = true
		}
		u.Unlock()

		select {
		case u.queue <- fn:
			// pass
		case <-exit:
			return
		}
	}
}

func (u *Uploader) watchWorker(exit chan bool) {
	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		select {
		case <-exit:
			return
		case <-t.C:
			u.watch(exit)
		}
	}
}
