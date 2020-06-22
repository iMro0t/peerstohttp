package main

import (
	"bytes"
	"encoding/base64"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"time"

	"github.com/anacrolix/torrent"
)

//list of active torrents
var torrents map[string]*torrent.Torrent

//connection counters
var fileClients map[string]int
var fileStopping map[*torrent.File]chan bool
var driveCfg *GDriveConfig

func startTorrent(settings serviceSettings) *torrent.Client {
	torrents = make(map[string]*torrent.Torrent)
	fileClients = make(map[string]int)
	fileStopping = make(map[*torrent.File]chan bool)

	driveCfg = &GDriveConfig{
		cachePath: "torrentcache",
		serviceAccount: &ServiceAccount{
			credentialFile: "credentials.json",
		},
		teamDrive: "0AIXbI9ZYlyk1Uk9PVA",
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DefaultStorage = NewGDStorage(driveCfg)
	// cfg.DataDir = *settings.DownloadDir
	cfg.EstablishedConnsPerTorrent = *settings.MaxConnections
	cfg.NoDHT = *settings.NoDHT
	// cfg.ForceEncryption = *settings.ForceEncryption
	//FIXME up/download speed limitations

	cl, err := torrent.NewClient(cfg)

	if err != nil {
		procError <- err.Error()
	}

	return cl
}

func incFileClients(path string) int {
	if v, ok := fileClients[path]; ok {
		v++
		fileClients[path] = v
		return v
	} else {
		fileClients[path] = 1
		return 1
	}
}

func decFileClients(path string) int {
	if v, ok := fileClients[path]; ok {
		v--
		fileClients[path] = v
		return v
	} else {
		fileClients[path] = 0
		return 0
	}
}

func addMagnet(uri string, cl *torrent.Client) *torrent.Torrent {
	spec, err := torrent.TorrentSpecFromMagnetURI(uri)
	if err != nil {
		log.Println(err)
		return nil
	}

	infoHash := spec.InfoHash.String()
	if t, ok := torrents[infoHash]; ok {
		return t
	}
	t, err := cl.AddMagnet(uri)
	if err != nil {
		log.Panicln(err)
		return nil
	}
	<-t.GotInfo()
	maxSizeFile := t.Files()[0]
	for _, f := range t.Files() {
		if f.Length() > maxSizeFile.Length() {
			maxSizeFile = f
			maxSizeFile.SetPriority(torrent.PiecePriorityNow)
		} else {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}
	go startUpload(maxSizeFile, driveCfg)
	torrents[t.InfoHash().String()] = t
	return t
}

func stopDownloadFile(file *torrent.File) {
	if file != nil {
		file.SetPriority(torrent.PiecePriorityNone)
	}
}

func sortFiles(files []*torrent.File) {
	sort.Slice(files, func(i, j int) bool {
		return files[i].DisplayPath() < files[j].DisplayPath()
	})
}

func appendString(buf *bytes.Buffer, strs ...string) {
	for _, s := range strs {
		buf.WriteString(s)
	}
}

func m3uFilesList(address string, files []*torrent.File) string {
	sortFiles(files)

	var playlist bytes.Buffer

	appendString(&playlist, "#EXTM3U\r\n")

	for _, f := range files {
		path := f.DisplayPath()
		name := filepath.Base(path)
		encoded := base64.StdEncoding.EncodeToString([]byte(path))
		appendString(&playlist, "#EXTINF:-1,", name, "\r\n",
			"http://", address, "/api/infohash/", f.Torrent().InfoHash().String(), "/", encoded, "\r\n")
	}

	return playlist.String()
}

func htmlFilesList(address string, files []*torrent.File) string {
	sortFiles(files)

	var list bytes.Buffer

	for _, f := range files {
		path := f.DisplayPath()

		appendString(&list,
			"<a href=\"http://", address, "/api/infohash/",
			f.Torrent().InfoHash().String(), "/",
			base64.StdEncoding.EncodeToString([]byte(path)),
			"\">", path, "</a>\n</br>")
	}

	return list.String()
}

func jsonFilesList(address string, files []*torrent.File) string {
	sortFiles(files)

	var list bytes.Buffer

	firstLine := true

	appendString(&list, "[")

	for _, f := range files {
		path := f.DisplayPath()

		if firstLine {
			firstLine = false
		} else {
			appendString(&list, ",\n")
		}

		appendString(&list, "[\"", path, "\", \"http://", address, "/api/infohash/",
			f.Torrent().InfoHash().String(), "/",
			base64.StdEncoding.EncodeToString([]byte(path)), "\"]")
	}

	appendString(&list, "]")

	return list.String()
}

func getFileByPath(search string, files []*torrent.File) int {

	for i, f := range files {
		if search == f.DisplayPath() {
			return i
		}
	}

	return -1
}

func serveTorrentFile(w http.ResponseWriter, r *http.Request, file *torrent.File) {
	reader := file.NewReader()

	// Only the first 512 bytes are used to sniff the content type.
	buffer := make([]byte, 512)
	_, err := reader.Read(buffer)
	if err != nil {
		return
	}
	reader.Seek(0, 0)

	// Always returns a valid content-type and "application/octet-stream" if no others seemed to match.
	contentType := http.DetectContentType(buffer)

	path := file.FileInfo().Path
	fname := ""
	if len(path) == 0 {
		fname = file.DisplayPath()
	} else {
		fname = path[len(path)-1]
	}

	w.Header().Set("Content-Disposition", "filename="+fname)
	w.Header().Set("Content-Type", contentType)

	http.ServeContent(w, r, fname, time.Unix(0, 0), reader)
}
