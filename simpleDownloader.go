package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"errors"
	"net/http"

	"github.com/cheggaaa/pb"
	"golang.org/x/net/proxy"
	"log"
	"sync"
)

const (
	appName               = "simpleDownloader"
	defaultOutputFileName = "default"
	CFG_FILENAME          = ".sh_cfg"
	CFG_DELIMETER         = "##"
	rbuffer_size          = 1024 * 1024
	wbuffer_size          = 100 * 1024 * 1024
	maxFileNameLen        = 128
)

type Cookie struct {
	Key, Val string
}

type Header struct {
	Header, Value string
}

var (
	allPiecesOk    bool
	wg             sync.WaitGroup
	tryThreadhold  int
	connNum        int
	userAgent      string
	showVersion    bool
	debug          bool
	urls           []string
	outputPath     string
	outputFileName string
	outputFile     *os.File
	contentLength  int
	acceptRange    bool
	bar            *pb.ProgressBar
	cookiePath     string
	usrDefHeader   string
	forcePiece     bool
	cfgPath        string
	proxyAddr      string
	dialer         proxy.Dialer
	chunkFiles     []string
)

type SortString []string

func (s SortString) Len() int {
	return len(s)
}

func (s SortString) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s SortString) Less(i, j int) bool {
	strI := strings.Split(s[i], ".part.")
	strJ := strings.Split(s[j], ".part.")
	numI, _ := strconv.Atoi(strI[1])
	numJ, _ := strconv.Atoi(strJ[1])
	return numI < numJ
}

func init() {
	flag.IntVar(&tryThreadhold, "t", 5, "Retry threshold")
	flag.IntVar(&connNum, "n", 5, "Specify the number of connections")
	flag.StringVar(&outputFileName, "O", defaultOutputFileName, `Set output filename.`)
	flag.StringVar(&userAgent, "U", appName, "Set user agent")
	flag.StringVar(&cfgPath, "c", ".", "Config file path")
	flag.BoolVar(&debug, "d", false, "Print debug infomation")
	flag.BoolVar(&forcePiece, "f", false, "Force goaxel to use multi-thread")
	flag.StringVar(&outputPath, "o", ".", "Set output directory.")
	flag.BoolVar(&showVersion, "V", false, "Print version and copyright")
	flag.StringVar(&cookiePath, "load-cookies", "", `Cookie file path, in the format, originally used by Netscape's cookies.txt`)
	flag.StringVar(&usrDefHeader, "header", "", `Double semicolon seperated header string`)
	flag.StringVar(&proxyAddr, "x", "", "Set Proxy,<Protocol://HOST:PORT>")
	proxy.RegisterDialerType("http", NewHttpProxy)
}

func downloadOnePiece(rangeFrom, pieceSize, alreadyHas int,
	u string, c []Cookie, h []Header, ofname string) {
	dlDone := false
	defer func() {
		if !dlDone {
			allPiecesOk = false
			Info.Printf("%s is not ok!\n", ofname)
		} else {
			Info.Printf("%s is done!\n", ofname)
		}
		wg.Done()
	}()

	f, err := os.OpenFile(ofname, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0664)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	for try := 0; try < tryThreadhold; try++ {
		Info.Printf("try time(s):%d, rangeFrom: %d, pieceSize:%d, alreadyHas:%d\n", try, rangeFrom, pieceSize, alreadyHas)

		resp, err := Get(u, c, h, rangeFrom, pieceSize, alreadyHas)
		if err != nil {
			Error.Println(err.Error())
			time.Sleep(time.Second)
			continue
		}

		written := writeContent(resp, f, pieceSize, alreadyHas)

		alreadyHas += written
		if alreadyHas == pieceSize {
			dlDone = true
			break
		}
	}
}

func writeContent(resp *http.Response, f *os.File, pieceSize, alreadyHas int) (written int) {
	fileOffset := alreadyHas
	data := make([]byte, rbuffer_size)
	written = 0

	defer resp.Body.Close()

	var n int
	var err error
	for {
		left := pieceSize - alreadyHas - written
		if left >= rbuffer_size {
			n, err = resp.Body.Read(data)
		} else {
			n, err = resp.Body.Read(data[:left])
		}

		if err != nil && err != io.EOF {
			Error.Println(err.Error())
			return
		}

		f.WriteAt(data[:n], int64(fileOffset))
		fileOffset += n
		written += n

		bar.Add(n)

		if err == io.EOF || fileOffset == pieceSize {
			return
		}
	}
}

func parseUrl(urlStr string) (path string, host string, e error) {
	if ok := strings.Contains(urlStr, "//"); ok != true {
		urlStr = "http://" + urlStr //scheme not specified,treat it as http
	}

	path = urlStr

	u, err := url.Parse(urlStr)
	if err != nil {
		fmt.Println("ERROR:", err.Error())
		e = err
		return
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		fmt.Println("xx", u.Scheme)
		e = errors.New("only support http/https")
		return
	}

	host = u.Host

	return
}

func fileSize(fileName string) int64 {
	if fi, err := os.Stat(fileName); err == nil {
		return fi.Size()
	}
	return 0
}

func divideAndDownload(u string, cookie []Cookie, header []Header) (realConn int, ps int) {
	var ofname string
	var startPos, remainder int
	realConn = connNum

	Info.Printf("acceptRange:%t, connNum:%d\n", acceptRange, connNum)
	if (acceptRange == false || realConn == 1) && !forcePiece { //need not split work
		realConn = 1
	}

	eachPieceSize := contentLength / realConn
	remainder = contentLength - eachPieceSize*realConn

	if eachPieceSize > remainder {
		ps = eachPieceSize
	} else {
		ps = remainder
	}

	chunkFiles = chunkFiles[:0]
	for i := 0; i < realConn; i++ {
		startPos = i * eachPieceSize
		ofname = fmt.Sprintf("%s.part.%d", outputFileName, startPos)
		chunkFiles = append(chunkFiles, outputPath+"/"+ofname)

		alreadyHas := int(fileSize(ofname))
		if alreadyHas > 0 {
			bar.Add(alreadyHas)
		}

		//the last piece,down addtional 'remainder',eg. split 9 to 4 + (4+'1')
		if i == realConn-1 {
			eachPieceSize += remainder
		}
		Info.Printf("%s starts at %d, already has %d, part size %d\n", ofname, startPos, alreadyHas, eachPieceSize)

		if alreadyHas >= eachPieceSize {
			continue
		}

		wg.Add(1)
		go downloadOnePiece(startPos, eachPieceSize, alreadyHas, u, cookie, header, ofname)
	}
	return
}

func mergeChunkFiles(ps int) {
	var n int
	var err error

	Info.Printf("chunkFiles:%s", chunkFiles)
	if err != nil {
		Error.Fatal("Merge chunk files failed :", err.Error())
	}

	buf_size := ps
	if ps > wbuffer_size {
		buf_size = wbuffer_size
	}
	buf := make([]byte, buf_size)

	for _, v := range chunkFiles {
		chunkFile, _ := os.Open(v)
		defer chunkFile.Close()

		chunkReader := bufio.NewReader(chunkFile)
		chunkWriter := bufio.NewWriter(outputFile)

		for {
			n, err = chunkReader.Read(buf)
			if err != nil && err != io.EOF {
				panic(err)
			}
			if n == 0 {
				break
			}
			if _, err = chunkWriter.Write(buf[:n]); err != nil {
				panic(err)
			}
		}

		if err = chunkWriter.Flush(); err != nil {
			panic(err)
		}
		os.Remove(v)
	}
}

func getContentInfomation(u string, c []Cookie, h []Header) (length int, accept bool, filename string) {
	length = -1

	resp, err := Get(u, c, h, 0, 0, 0)
	if err != nil {
		Error.Fatal(err.Error())
	}
	defer resp.Body.Close()

	Info.Println("getContentInfomation resp header -->", resp.Header)
	length, _ = strconv.Atoi(resp.Header.Get("Content-Length"))
	if len(resp.Header.Get("Content-Range")) > 0 || len(resp.Header.Get("Accept-Ranges")) > 0 {
		accept = true
	}

	cd := resp.Header.Get("Content-Disposition")
	if len(cd) == 0 {
		return
	}

	key := `filename="`
	idx := strings.Index(cd, key)

	if -1 == idx {
		return
	}
	filename = cd[idx+len(key) : len(cd)-1]

	return
}

func loadUsrDefHeaders(h []Header, req *http.Request) {
	defer func() {
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", userAgent)
		}
	}()

	if len(h) == 0 {
		return
	}

	for _, v := range h {
		req.Header.Add(v.Header, v.Value)
	}
}

func loadUsrDefCookies(c []Cookie, req *http.Request) {
	if len(c) == 0 {
		return
	}

	cookie := ""
	for _, v := range c {
		cookie += v.Key + "=" + v.Val + ";"
	}
	cookie = cookie[:len(cookie)-1] //remove the last ';'
	req.Header.Add("Cookie", cookie)
}

func Get(path string, c []Cookie, h []Header, rangeFrom, pieceSize, alreadyHas int) (resp *http.Response, e error) {
	var client *http.Client
	var transport *http.Transport
	if dialer != nil {
		transport = &http.Transport{
			Proxy:               nil,
			Dial:                dialer.Dial,
			TLSHandshakeTimeout: 30 * time.Second,
		}
	}

	if transport != nil {
		client = &http.Client{Transport: transport}
	} else {
		client = &http.Client{}
	}

	req, err := http.NewRequest("GET", path, nil)
	if err != nil {
		e = err
		Error.Println(err.Error())
		return
	}

	req.Header.Set("Connection", "close") // default value of HTTP1.1 is 'keep-alive'
	if pieceSize == 0 {
		req.Header.Add("Range", "bytes=0-")
	} else {
		rangeFrom += alreadyHas
		req.Header.Add("Range", fmt.Sprintf("bytes=%d-%d", rangeFrom, rangeFrom+pieceSize-1))
	}

	loadUsrDefHeaders(h, req)
	loadUsrDefCookies(c, req)

	resp, e = client.Do(req)
	if err != nil {
		Error.Println(err.Error())
	}

	return
}

func createProgressBar(length int) (bar *pb.ProgressBar) {
	bar = pb.New(length)
	bar.ShowSpeed = true
	bar.Units = pb.U_BYTES
	return
}

func parseCookieLine(s []byte, host string) (c Cookie, ok bool) {
	line := strings.Split(string(s), "\t")
	if len(line) != 7 {
		return
	}

	if len(line[0]) < 3 {
		return
	}

	if line[0][0] == '#' {
		return
	}

	domain := line[0]
	if domain[0] == '.' {
		domain = domain[1:]
	}

	//Curl source says, quoting Andre Garcia: "flag: A TRUE/FALSE
	// value indicating if all machines within a given domain can
	// access the variable.  This value is set automatically by the
	// browser, depending on the value set for the domain."
	//domainFlag := line[1]

	//path := line[2]
	//security := line[3]
	expires := line[4]
	name := line[5]
	value := line[6]

	if false == strings.Contains(host, domain) {
		return
	}

	expiresTimestamp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil {
		fmt.Println("expires timestamp convert error :", err)
		return
	}

	if expiresTimestamp < time.Now().Unix() {
		return
	}

	c.Key = name
	c.Val = value
	ok = true
	return
}

func loadUsrDefinedHeader(usrDef string) (header []Header) {
	s := strings.Split(usrDef, ";;")
	for i := 0; i < len(s); i++ {
		l := len(s[i])
		if l < 3 {
			continue
		}

		kv := strings.Split(s[i], ":")
		if len(kv) < 2 {
			continue
		}

		//Referer:http://www.google.com
		idx := strings.Index(s[i], ":")
		if -1 == idx {
			return
		}
		val := s[i][idx+1 : l]
		header = append(header, Header{Header: kv[0], Value: val})
	}
	return
}

func loadCookies(cookiePath, host string) (cookie []Cookie, ok bool) {
	if cookiePath == "" {
		ok = false
		return
	}

	f, err := os.Open(cookiePath)
	if err != nil {
		fmt.Println("ERROR OPEN COOKIE :", err)
		ok = false
		return
	}
	defer f.Close()

	br := bufio.NewReader(f)
	for {
		line, isPrefix, err := br.ReadLine()
		if err != nil && err != io.EOF {
			ok = false
			return
		}
		if isPrefix {
			fmt.Println("you should not see this message")
			continue
		}

		if c, ok := parseCookieLine(line, host); ok {
			cookie = append(cookie, c)
		}

		if err == io.EOF {
			break
		}
	}

	ok = true
	return
}

func downSingleFile(urlStr string) bool {
	var err error
	var host string

	urlStr, host, err = parseUrl(urlStr)
	if err != nil {
		Error.Println(err.Error())
		return false
	}

	cookies, ok := loadCookies(cookiePath, host)
	if !ok {
		cookies = make([]Cookie, 0)
	}

	headers := loadUsrDefinedHeader(usrDefHeader)

	var headerFilename string
	contentLength, acceptRange, headerFilename = cfgRead(urlStr)
	if -1 == contentLength { // init firstly
		contentLength, acceptRange, headerFilename = getContentInfomation(urlStr, cookies, headers)

		if "" != headerFilename && outputFileName == defaultOutputFileName {
			outputFileName = headerFilename
		}

		checkOutputFileName(urlStr)

		if contentLength < 1 { // get failed
			Error.Printf("Link %s get content info failed\n", urlStr)
			getUnknownSizeFile(urlStr, cookies, headers, outputFileName)
			return false
		}

		cfgWrite(urlStr, contentLength, acceptRange, outputFileName)
	} else { // read from config
		outputFileName = headerFilename
		checkOutputFileName(urlStr)
	}

	Info.Printf("[DEBUG] content length:%d,accept range:%t, cookie file:%s, outputFileName:%s\n",
		contentLength, acceptRange, cookiePath, outputFileName)

	bar = createProgressBar(contentLength)
	defer bar.Finish()

	if outputFile, err = os.Create(outputFileName); err != nil {
		Info.Println("error create:", outputFile, ",link:", urlStr, ",error:", err.Error(), ",name:", len(outputFileName))
		return false
	}
	defer outputFile.Close()

	allPiecesOk = true
	_, pieceSize := divideAndDownload(urlStr, cookies, headers)
	bar.Start()

	wg.Wait()
	if !allPiecesOk {
		return false
	}

	mergeChunkFiles(pieceSize)
	cfgDelete(urlStr)
	return true
}

func showVersionInfo() {
	fmt.Println(fmt.Sprintf("%s Version 1.1", appName))
	fmt.Println("Copyright (C) 2017 kumakichi")
}

func showUsage() {
	fmt.Printf("Usage: %s [options] url1 [url2] [url...]\n", appName)
	flag.PrintDefaults()
}

func checkUrls(u *[]string) {
	if len(urls) == 0 {
		if false == showVersion {
			Error.Fatal("You must specify at least one url to download")
		}
	}

	if len(urls) > 1 { // more than 1 url,can not set ouputfile name
		outputFileName = defaultOutputFileName
	}
}

func changeToOutputDir(dst string) {
	if dst != "." {
		if err := os.Chdir(dst); err != nil {
			Error.Fatal("Change directory failed :", dst, err)
		}
	}
}

func getCookieAbsolutePath() {
	var err error

	if cookiePath == "" {
		return
	}

	cookiePath, err = filepath.Abs(cookiePath)
	if err != nil {
		cookiePath = ""
		Error.Fatal("Error get absolute path :", cookiePath)
	}
}

func main() {
	if len(os.Args) == 1 {
		showUsage()
		return
	}

	flag.Parse()

	if showVersion {
		showVersionInfo()
	}

	if len(proxyAddr) > 0 { // use sokcs
		var err error
		var proxyUrl *url.URL

		proxyUrl, err = url.Parse(proxyAddr)
		if err != nil {
			log.Printf("Failed to parse proxy URL: %v\n", err)
			os.Exit(-1)
		}

		dialer, err = proxy.FromURL(proxyUrl, proxy.Direct)
		if err != nil {
			fmt.Println(err)
			Error.Println(err.Error())
		}
	}

	initLog()

	urls = flag.Args()
	checkUrls(&urls)

	getCookieAbsolutePath()
	changeToOutputDir(outputPath)

	for i := 0; i < len(urls); i++ {
		downSingleFile(urls[i])
	}
}

func getUnknownSizeFile(path string, c []Cookie, h []Header, outname string) {
	resp, err := Get(path, c, h, 0, 0, 0)
	if err != nil {
		Error.Println(err.Error())
		return
	}
	defer resp.Body.Close()

	f, err := os.OpenFile(outname, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0664)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	var n int
	data := make([]byte, rbuffer_size)
	fileOffset := 0

	fmt.Println("Downloading unknown size file, please wait ...")
	for {
		n, err = resp.Body.Read(data)

		if err != nil && err != io.EOF {
			Error.Println(err.Error())
			return
		}

		f.WriteAt(data[:n], int64(fileOffset))
		fileOffset += n

		if err == io.EOF {
			fmt.Println("Done")
			return
		}
	}
}

func checkOutputFileName(url_path string) {
	if outputFileName == defaultOutputFileName {
		outputFileName = path.Base(url_path)
	}
	outputFileName = path.Base(outputFileName)

	l := len(outputFileName)
	if l > maxFileNameLen {
		outputFileName = outputFileName[l-maxFileNameLen : l]
	}

	if stat, err := os.Stat(outputFileName); err != nil {
		if os.IsNotExist(err) {
			return
		}
	} else {
		if stat.Size() > 0 {
			log.Printf("File \"%s\" already exist, try another name", outputFileName)
			os.Exit(-1)
		}
	}
}
