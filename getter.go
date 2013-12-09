package s3gof3r

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"sync"
	"syscall"
)

type getter struct {
	url    url.URL
	client *http.Client
	b      *Bucket
	bufsz  int64
	err    error
	wg     sync.WaitGroup

	cur_chunk_id   int
	cur_chunk      *chunk
	content_length int64
	chunk_total    int
	read_ch        chan *chunk
	get_ch         chan *chunk

	get   chan *bytes.Buffer
	give  chan *bytes.Buffer
	makes int

	q_wait map[int]*chunk

	concurrency int
	nTry        int
	closed      bool
	c           *Config

	md5 hash.Hash
}

type chunk struct {
	id     int
	header http.Header
	start  int64
	size   int64
	b      *bytes.Buffer
	len    int64
}

func newGetter(p_url url.URL, c *Config, b *Bucket) (io.ReadCloser, http.Header, error) {
	// initialize getter
	g := new(getter)
	g.url = p_url
	g.bufsz = c.PartSize
	//g.bp.get, g.bp.give = makeRecycler()
	g.get_ch = make(chan *chunk)
	g.read_ch = make(chan *chunk)
	g.nTry = c.NTry
	g.concurrency = c.Concurrency
	g.q_wait = make(map[int]*chunk)
	g.b = b
	g.c = c
	g.client = c.Client
	g.md5 = md5.New()

	// get content length
	r, err := http.NewRequest("HEAD", p_url.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	g.b.Sign(r)
	resp, err := retryRequest(r, g.client, g.nTry)
	defer resp.Body.Close()
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != 200 {
		return nil, nil, newRespError(resp)
	}
	g.content_length = resp.ContentLength
	g.concurrency = int(min64(int64(g.concurrency), (g.content_length/g.bufsz))) + 1
	//start buffer pool with size of concurrency
	g.get, g.give = startBufferPool(g.concurrency * 2)

	for i := 0; i < g.concurrency; i++ {
		go g.worker()
	}
	go g.init_chunks()
	return g, resp.Header, nil
}

func (g *getter) init_chunks() {
	for i := int64(0); i < g.content_length; {
		size := min64(g.bufsz, g.content_length-i)
		c := &chunk{
			id: g.chunk_total,
			header: http.Header{
				"Range": {fmt.Sprintf("bytes=%d-%d",
					i, i+size-1)},
			},
			start: i,
			size:  size,
			b:     nil,
			len:   0}
		i += size
		g.chunk_total++
		g.wg.Add(1)
		g.get_ch <- c
	}
}

func (g *getter) worker() {
	for c := range g.get_ch {
		g.retryGetChunk(c)
	}

}

func (g *getter) retryGetChunk(c *chunk) {
	defer g.wg.Done()
	var err error
	c.b = g.get_buffer()
	for i := 0; i < g.nTry; i++ {
		err = g.getChunk(c)
		if err == nil {
			return
		}
		log.Printf("Error on attempt %d: retrying chunk: %v, Error: %s", i, c, err)
	}
	g.err = err
}

func (g *getter) get_buffer() *bytes.Buffer {
	var b *bytes.Buffer
	if g.makes < g.concurrency*2 && len(g.get) == 0 {
		size := g.bufsz + 1*kb
		s := make([]byte, 0, size)
		b = bytes.NewBuffer(s)
		g.makes++
	} else {
		b = <-g.get
	}
	return b
}

func (g *getter) getChunk(c *chunk) error {
	// ensure buffer is empty
	c.b.Reset()

	r, err := http.NewRequest("GET", g.url.String(), nil)
	if err != nil {
		return err
	}
	//r.Close = true
	r.Header = c.header
	g.b.Sign(r)
	resp, err := g.client.Do(r)
	defer resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode != 206 {
		return newRespError(resp)
	}
	n, err := c.b.ReadFrom(resp.Body)
	if err != nil {
		return err
	}
	if n != c.size {
		return fmt.Errorf("Chunk %d: Expected %d bytes, received %d",
			c.id, c.size, n)
	}
	g.read_ch <- c
	return nil
}

func (g *getter) Read(p []byte) (int, error) {
	var err error
	if g.closed {
		return 0, syscall.EINVAL
	}
	if g.err != nil {
		return 0, g.err
	}
	if g.cur_chunk == nil {
		g.cur_chunk, err = g.get_cur_chunk()
		if err != nil {
			return 0, err
		}
	}
	// write to md5 hash in parallel with output
	tr := io.TeeReader(g.cur_chunk.b, g.md5)
	//n, err := g.cur_chunk.b.Read(p)
	n, err := tr.Read(p)

	// Empty buffer, move on to next
	if err == io.EOF {
		// Do not send EOF for each chunk.
		if g.cur_chunk.id == g.chunk_total-1 && g.cur_chunk.b.Len() == 0 {
			return n, err // end of stream, send eof
		}
		g.give <- g.cur_chunk.b // recycle buffer
		g.cur_chunk = nil
		g.cur_chunk_id++
		return n - 1, nil // subtract EOF
	}
	return n, err
}

func (g *getter) get_cur_chunk() (*chunk, error) {
	var cur_chunk *chunk
	var err error
	for {
		// first check q_wait
		if cur_chunk, ok := g.q_wait[g.cur_chunk_id]; ok {
			delete(g.q_wait, g.cur_chunk_id)
			return cur_chunk, err
		}
		// if not present, read from channel
		cur_chunk = <-g.read_ch
		g.q_wait[cur_chunk.id] = cur_chunk
	}
}

func (g *getter) Close() error {
	if g.closed {
		return syscall.EINVAL
	}
	if g.err != nil {
		return g.err
	}
	g.wg.Wait()
	close(g.read_ch)
	close(g.get_ch)
	g.closed = true
	log.Println("makes:", g.makes)
	if g.c.Md5Check {
		if err := g.checkMd5(); err != nil {
			return err
		}
	}
	return nil
}

func (g *getter) checkMd5() (err error) {
	calcMd5 := fmt.Sprintf("%x", g.md5.Sum(nil))
	md5Path := fmt.Sprint(".md5", g.url.Path, ".md5")
	md5Url := g.b.Url(md5Path, g.c)
	r, err := http.NewRequest("GET", md5Url.String(), nil)
	if err != nil {
		return
	}
	log.Println("md5: ", calcMd5)
	log.Println("md5Path: ", md5Path)
	g.b.Sign(r)
	resp, err := retryRequest(r, g.client, g.nTry)
	defer resp.Body.Close()
	if err != nil {
		return
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("MD5 check failed: %s not found: %s", md5Url, newRespError(resp))
	}
	givenMd5, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}
	if calcMd5 != string(givenMd5) {
		return fmt.Errorf("MD5 mismatch. given:%s calculated:%s", givenMd5, calcMd5)
	}
	return
}
