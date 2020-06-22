package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"path"
	"sort"
	"strconv"

	"github.com/anacrolix/torrent"

	"github.com/anacrolix/missinggo/v2/filecache"
	"github.com/anacrolix/missinggo/v2/resource"
	"github.com/anacrolix/missinggo/x"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

type piecePerResource struct {
	p resource.Provider
}

type ServiceAccount struct {
	credentialFile string
	authClient     *http.Client
}

type GDriveConfig struct {
	cachePath      string
	serviceAccount *ServiceAccount
	teamDrive      string
}

func NewGDStorage(cfg *GDriveConfig) storage.ClientImplCloser {
	return NewResourcePieces(getStorageProvider(cfg.cachePath))
}

func NewResourcePieces(p resource.Provider) storage.ClientImplCloser {
	return &piecePerResource{
		p: p,
	}
}

func (s *piecePerResource) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	return s, nil
}

func (s *piecePerResource) Close() error {
	return nil
}

func (s *piecePerResource) Piece(p metainfo.Piece) storage.PieceImpl {
	return piecePerResourcePiece{
		mp: p,
		rp: s.p,
	}
}

type piecePerResourcePiece struct {
	mp metainfo.Piece
	rp resource.Provider
}

func (s piecePerResourcePiece) Completion() storage.Completion {
	fi, err := s.completed().Stat()
	return storage.Completion{
		Complete: err == nil && fi.Size() == s.mp.Length(),
		Ok:       true,
	}
}

func (s piecePerResourcePiece) MarkComplete() error {
	incompleteChunks := s.getChunks()
	err := s.completed().Put(io.NewSectionReader(incompleteChunks, 0, s.mp.Length()))
	if err == nil {
		for _, c := range incompleteChunks {
			c.instance.Delete()
		}
	}
	return err
}

func (s piecePerResourcePiece) MarkNotComplete() error {
	return s.completed().Delete()
}

func (s piecePerResourcePiece) ReadAt(b []byte, off int64) (int, error) {
	if s.Completion().Complete {
		return s.completed().ReadAt(b, off)
	}
	return s.getChunks().ReadAt(b, off)
}

func (s piecePerResourcePiece) WriteAt(b []byte, off int64) (n int, err error) {
	i, err := s.rp.NewInstance(path.Join(s.incompleteDirPath(), strconv.FormatInt(off, 10)))
	if err != nil {
		panic(err)
	}
	r := bytes.NewReader(b)
	err = i.Put(r)
	n = len(b) - r.Len()
	return
}

type chunk struct {
	offset   int64
	instance resource.Instance
}

type chunks []chunk

func (me chunks) ReadAt(b []byte, off int64) (int, error) {
	for {
		if len(me) == 0 {
			return 0, io.EOF
		}
		if me[0].offset <= off {
			break
		}
		me = me[1:]
	}
	n, err := me[0].instance.ReadAt(b, off-me[0].offset)
	if n == len(b) {
		return n, nil
	}
	if err == nil || err == io.EOF {
		n_, err := me[1:].ReadAt(b[n:], off+int64(n))
		return n + n_, err
	}
	return n, err
}

func (s piecePerResourcePiece) getChunks() (chunks chunks) {
	names, err := s.incompleteDir().Readdirnames()
	if err != nil {
		return
	}
	for _, n := range names {
		offset, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			continue
		}
		i, err := s.rp.NewInstance(path.Join(s.incompleteDirPath(), n))
		if err != nil {
			panic(err)
		}
		chunks = append(chunks, chunk{offset, i})
	}
	sort.Slice(chunks, func(i, j int) bool {
		return chunks[i].offset < chunks[j].offset
	})
	return
}

func (s piecePerResourcePiece) completed() resource.Instance {
	i, err := s.rp.NewInstance(path.Join("completed", s.mp.Hash().HexString()))
	if err != nil {
		panic(err)
	}
	return i
}

func (s piecePerResourcePiece) incompleteDirPath() string {
	return path.Join("incompleted", s.mp.Hash().HexString())
}

func (s piecePerResourcePiece) incompleteDir() resource.DirInstance {
	i, err := s.rp.NewInstance(s.incompleteDirPath())
	if err != nil {
		panic(err)
	}
	return i.(resource.DirInstance)
}

func getStorageProvider(cachePath string) resource.Provider {
	fc, err := filecache.NewCache(cachePath)
	x.Pie(err)

	fc.SetCapacity(int64(100 << 20))
	return fc.AsResourceProvider()
}

type File struct {
	reader       torrent.Reader
	id           string
	mime         string
	length       int64
	torrentFile  *torrent.File
	parentID     string
	resumableURI string
	done         chan bool
}

type MetaData struct {
	id       string   `json:"id"`
	mimeType string   `json:"mimeType"`
	name     string   `json:"name"`
	parents  []string `json:"parents"`
}

func startUpload(f *torrent.File, cfg *GDriveConfig) <-chan bool {
	srv := cfg.serviceAccount
	auth(srv)
	file := &File{
		torrentFile: f,
		reader:      f.NewReader(),
		length:      f.Length(),
		parentID:    "root",
	}
	if cfg.teamDrive != "" {
		file.parentID = cfg.teamDrive
	}
	// createFolder(cfg, srv, file)
	initUpload(file, srv)
	continueUpload(file, srv)
	return file.done
}

func initUpload(f *File, srv *ServiceAccount) {
	detetctMime(f)
	generateID(f, srv)

	metadata, _ := json.Marshal(map[string]interface{}{
		"id":       f.id,
		"mimeType": f.mime,
		"name":     f.torrentFile.DisplayPath(),
		"parents":  []string{f.parentID},
	})
	req, err := http.NewRequest("POST", "https://www.googleapis.com/upload/drive/v3/files?uploadType=resumable&supportsAllDrives=true", bytes.NewBuffer(metadata))
	req.Header.Add("Content-Type", "application/json; charset=UTF-8")
	req.Header.Add("X-Upload-Content-Type", f.mime)
	// req.Header.Add("X-Upload-Content-Length", strconv.FormatInt(f.length, 10))
	resp, err := srv.authClient.Do(req)
	if err != nil {
		log.Fatalln(err)
	}
	defer resp.Body.Close()
	f.resumableURI = resp.Header.Get("Location")
	log.Println("Uploading initialized with resumableURI", f.resumableURI)
}

type GDUploadChunk struct {
	startOff int
	endOff   int
	chunk    *bytes.Buffer
	eof      bool
}

var uploadChan chan *GDUploadChunk

func continueUpload(f *File, srv *ServiceAccount) {
	go func() {
		uploadChan = make(chan *GDUploadChunk)
		go uploadChunk(f, srv, uploadChan)
		startOff := 0
		buf := make([]byte, 16*1024*1024)
		for {
			n, err := f.reader.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Fatal(err)
				}
				uploadChan <- &GDUploadChunk{
					startOff: startOff,
					endOff:   startOff + n - 1,
					chunk:    bytes.NewBuffer(buf[:n]),
					eof:      true,
				}
				close(uploadChan)
				f.reader.Close()
				break
			}
			uploadChan <- &GDUploadChunk{
				startOff: startOff,
				endOff:   startOff + n - 1,
				chunk:    bytes.NewBuffer(buf),
				eof:      false,
			}
			startOff += n
		}
		f.done <- true
	}()
}

func uploadChunk(f *File, srv *ServiceAccount, uploadChan chan *GDUploadChunk) {
	for uchunk := range uploadChan {
		req, _ := http.NewRequest("PUT", f.resumableURI, uchunk.chunk)
		var crange string
		if uchunk.eof {
			crange = fmt.Sprintf("bytes %d-%d/%d", uchunk.startOff, uchunk.endOff, uchunk.endOff+1)
		} else {
			crange = fmt.Sprintf("bytes %d-%d/*", uchunk.startOff, uchunk.endOff)
		}
		req.Header.Add("Content-Range", crange)
		log.Println("Uploading", crange)
		resp, err := srv.authClient.Do(req)
		if err != nil {
			log.Fatal(err)
		}
		if !(resp.StatusCode == 200 || resp.StatusCode == 201 || resp.StatusCode == 308) {
			defer resp.Body.Close()
			body, _ := ioutil.ReadAll(resp.Body)
			log.Println("Response code", resp.Status)
			log.Println(string(body))
		}
		log.Println("Response range", resp.Header.Get("Range"))
	}
}

func generateID(f *File, srv *ServiceAccount) {
	req, _ := http.NewRequest("GET", "https://www.googleapis.com/drive/v3/files/generateIds?count=1", nil)
	resp, err := srv.authClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	if resp.StatusCode == 200 {
		var result map[string]interface{}
		json.Unmarshal(body, &result)
		f.id = result["ids"].([]interface{})[0].(string)
		log.Println("Generated Id", f.id)
	} else {
		panic(string(body))
	}
}

func createFolder(cfg *GDriveConfig, srv *ServiceAccount, f *File) {
	var driveRoot string
	if cfg.teamDrive == "" {
		driveRoot = "root"
	} else {
		driveRoot = cfg.teamDrive
	}
	metadata, _ := json.Marshal(map[string]interface{}{
		"mimeType": "application/vnd.google-apps.folder",
		"name":     f.torrentFile.Torrent().InfoHash().String(),
		"parents":  []string{driveRoot},
	})
	resp, err := srv.authClient.Post("https://www.googleapis.com/drive/v3/files?supportsAllDrives=true", "application/json", bytes.NewBuffer(metadata))
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	if resp.StatusCode == 200 {
		var result map[string]interface{}
		json.Unmarshal(body, &result)
		f.parentID = result["id"].(string)
		log.Println("Folder created in", driveRoot, "with id", f.parentID)
	} else {
		panic(string(body))
	}
}

func auth(srv *ServiceAccount) {
	b, err := ioutil.ReadFile(srv.credentialFile)
	if err != nil {
		log.Fatal(err)
	}
	var c = struct {
		Email      string `json:"client_email"`
		PrivateKey string `json:"private_key"`
	}{}
	json.Unmarshal(b, &c)
	config := &jwt.Config{
		Email:      c.Email,
		PrivateKey: []byte(c.PrivateKey),
		Scopes: []string{
			"https://www.googleapis.com/auth/drive",
		},
		TokenURL: "https://oauth2.googleapis.com/token",
	}
	srv.authClient = config.Client(oauth2.NoContext)
}

func detetctMime(f *File) {
	// Only the first 512 bytes are used to sniff the content type.
	buffer := make([]byte, 512)
	_, err := f.reader.Read(buffer)
	if err != nil {
		panic(err)
	}
	f.reader.Seek(0, 0)
	f.mime = http.DetectContentType(buffer)
}
