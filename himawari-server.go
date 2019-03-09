package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/google/uuid"
	lsd "github.com/mattn/go-lsd"
	log "github.com/sirupsen/logrus"
)

const (
	HTTP_PORT      = 10616
	HTTP_DIR       = "./public_html"
	RAW_PATH       = "/data/video/tmp"
	DELETE_PATH    = "/data/video/del"
	ENCODED_PATH   = "/data/public/video"
	THUMBNAIL_PATH = "/data/public/thumbnail"

	WORKER_CHECK_DURATION     = time.Hour
	WORKER_DELETE_DURATION    = time.Hour * 24
	COMPLETED_DELETE_DURATION = time.Hour * 24 * 7

	PRESET_PREFIX               = "libx265-hq-ts_"
	PRESET_EXT                  = ".ffpreset"
	ENCODE_THREADS              = 0
	THUMBNAIL_INTERVAL_DURATION = time.Second * 10
)

const PRESET_DATA = `level=41
crf=23
coder=1
flags=+loop
partitions=all
me_method=umh
subq=8
trellis=2
psy-rd==0.5:0.0
aq-strength=0.8
me_range=16
g=300
keyint_min=25
sc_threshold=50
i_qfactor=0.71
b_strategy=2
b_adapt=2
qmin=10
rc_eq='blurCplx^(1-qComp)'
bf=16
bidir_refine=1
refs=16
deblock=0:0`

type Task struct {
	Id       string
	Size     int64
	Name     string
	Category string
	Title    string
	Subtitle string
	Duration time.Duration
	rp       string // raw
	ep       string // encoded
	dp       string // delete
	tp       string // thumbnail
}
type Worker struct {
	Task  *Task
	Host  string
	Start time.Time
	End   time.Time
}
type Thumbnail struct {
	d  time.Duration
	ep string
	tp string
}
type himawariTask struct {
	all <-chan []*Task
	pop <-chan *Task
	add chan<- *Task
}
type himawariWorkerAddItem struct {
	id string
	w  Worker
}
type himawariWorker struct {
	all <-chan map[string]Worker
	add chan<- himawariWorkerAddItem
	del chan<- string
}
type himawariComplete struct {
	all <-chan []Worker
	add chan<- Worker
}

type himawariHandle struct {
	file      http.Handler
	thumbc    chan<- Thumbnail
	tasks     himawariTask
	worker    himawariWorker
	completed himawariComplete
}
type Dashboard struct {
	Tasks     []*Task
	Worker    map[string]Worker
	Completed []Worker
}

var regFilename = regexp.MustCompile(`^\[(\d{6}-\d{4})\]\[([^\]]+)\]\[([^\]]+)\]\[([^\]]+)\]\[([^\]]+)\](.+?)_\[(.*?)\]_\[(.*?)\]\.m2ts$`)
var serverIP string

func init() {
	log.SetFormatter(&log.TextFormatter{})
	log.SetLevel(log.InfoLevel)

	var err error
	serverIP, err = externalIP()
	if err != nil {
		log.WithError(err).Fatal("自身のIPアドレスの取得に失敗しました。")
	}
}

func main() {
	hh := NewHimawari()
	h := &http.Server{
		Addr:    fmt.Sprintf(":%d", HTTP_PORT),
		Handler: hh,
	}
	log.WithError(h.ListenAndServe()).Fatal("サーバが停止しました。")
}

func (hh *himawariHandle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := path.Clean(r.URL.Path)
	if strings.Index(p, "/video/id/") == 0 {
		if r.Method == "GET" {
			id := p[10:]
			wo, ok := (<-hh.worker.all)[id]
			if ok {
				rfp, err := os.Open(wo.Task.rp)
				if err != nil {
					// そんなファイルはない
					http.NotFound(w, r)
				} else {
					defer rfp.Close()
					http.ServeContent(w, r, wo.Task.Name, wo.Start, rfp)
				}
			} else {
				// そんな仕事はない
				http.NotFound(w, r)
			}
		} else {
			http.Error(w, "GET以外のメソッドには対応していません。", http.StatusMethodNotAllowed)
			log.WithFields(log.Fields{
				"path":   r.URL.Path,
				"method": r.Method,
			}).Info("対応していないメソッドです。")
		}
	} else if p == "/task" {
		// お仕事
		switch r.Method {
		case "GET":
			// お仕事を得る
			t := hh.taskToWorker(r)
			if t != nil {
				pname := PRESET_PREFIX + t.Id + PRESET_EXT
				tt := struct {
					Task
					PresetName string
					PresetData string
					Command    string
					Args       []string
				}{
					Task:       *t,
					PresetName: pname,
					PresetData: PRESET_DATA,
					Command:    "ffmpeg",
					Args: []string{
						"-y",
						"-i", fmt.Sprintf("http://%s:%d/video/id/%s", serverIP, HTTP_PORT, t.Id),
						"-threads", strconv.FormatInt(getHeaderDec(r, "X-Himawari-Threads", ENCODE_THREADS), 10),
						"-fpre", pname,
						"-vcodec", "libx265",
						"-acodec", "aac", // libfdk_aac
						"-ar", "48000",
						"-ab", "128k",
						"-r", "30000/1001",
						"-s", "1280x720",
						"-vsync", "1",
						"-deinterlace",
						"-pix_fmt", "yuv420p",
						"-f", "mp4",
						"-bufsize", "200000k",
						"-maxrate", "2000k",
					},
				}
				buf := bytes.Buffer{}
				err := json.NewEncoder(&buf).Encode(&tt)
				if err == nil {
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.Header().Set("X-Content-Type-Options", "nosniff")
					w.WriteHeader(http.StatusOK)
					n, err := buf.WriteTo(w)
					if err != nil {
						log.WithFields(log.Fields{
							"path": r.URL.Path,
							"size": tt.Size,
							"name": tt.Name,
						}).Info("お仕事の転送に失敗しました。", err)
					} else {
						log.WithFields(log.Fields{
							"path": r.URL.Path,
							"send": n,
						}).Info("お仕事の転送に成功しました。")
					}
				} else {
					// お仕事やり直し
					hh.workerToTask(t.Id)
					http.Error(w, "お仕事のjsonエンコードに失敗しました。", http.StatusInternalServerError)
					log.WithFields(log.Fields{
						"path": r.URL.Path,
						"size": tt.Size,
						"name": tt.Name,
					}).Info("お仕事のjsonエンコードに失敗しました。", err)
				}
			} else {
				// お仕事はない
				http.NotFound(w, r)
				log.WithFields(log.Fields{
					"path":   r.URL.Path,
					"method": r.Method,
				}).Info("仕事がありません。")
			}
		default:
			http.Error(w, "GET、POST以外のメソッドには対応していません。", http.StatusMethodNotAllowed)
			log.WithFields(log.Fields{
				"path":   r.URL.Path,
				"method": r.Method,
			}).Info("対応していないメソッドです。")
		}
	} else if p == "/task/add" {
		// お仕事を追加する
		if r.Method == "POST" {
			stat, err := os.Stat(filepath.Join(RAW_PATH, r.PostFormValue("filename")))
			if err == nil {
				t := NewTask(stat)
				if t != nil {
					// 追加
					hh.tasks.add <- t
				} else {
					// 失敗しても特にエラーではない
				}
				w.WriteHeader(http.StatusOK)
			} else {
				http.Error(w, "リクエストされたファイルが存在しないようです。", http.StatusBadRequest)
			}
		} else {
			http.Error(w, "POST以外のメソッドには対応していません。", http.StatusMethodNotAllowed)
			log.WithFields(log.Fields{
				"path":   r.URL.Path,
				"method": r.Method,
			}).Info("対応していないメソッドです。")
		}
	} else if p == "/task/done" {
		// お仕事完了
		if r.Method == "POST" {
			err := hh.done(r)
			if err == nil {
				// 本当に完了
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
			} else {
				// 完了に失敗
				// やり直し判断は定期処理に任せる
				http.Error(w, "なんか失敗しました。", http.StatusInternalServerError)
			}
		} else {
			http.Error(w, "POST以外のメソッドには対応していません。", http.StatusMethodNotAllowed)
			log.WithFields(log.Fields{
				"path":   r.URL.Path,
				"method": r.Method,
			}).Info("対応していないメソッドです。")
		}
	} else if strings.Index(p, "/task/id/") == 0 {
		// 実装は後で
		http.NotFound(w, r)
	} else if p == "/" || p == "/index.html" {
		// トップページの表示
		hh.dashboard(w, r)
	} else {
		// ファイルサーバにお任せ
		hh.file.ServeHTTP(w, r)
	}
}

func (hh *himawariHandle) dashboard(w http.ResponseWriter, r *http.Request) {
	db := Dashboard{
		Tasks:     <-hh.tasks.all,
		Worker:    <-hh.worker.all,
		Completed: <-hh.completed.all,
	}
	tmpl := template.New("t")
	tmpl.Funcs(template.FuncMap{
		"ShortByte": func(s int64) (ret string) {
			if s > 1000*1000*1000 {
				ret = fmt.Sprintf("%.2fGB", float64(s)/(1000*1000*1000))
			} else {
				ret = fmt.Sprintf("%.2fMB", float64(s)/(1000*1000))
			}
			return
		},
	})
	template.Must(tmpl.ParseFiles(filepath.Join(HTTP_DIR, "index.html")))
	if err := tmpl.ExecuteTemplate(w, "index.html", db); err != nil {
		log.WithError(err).Warn("index.htmlの生成に失敗しました。")
	}
}

func (hh *himawariHandle) done(r *http.Request) error {
	id := r.PostFormValue("uuid")

	wo, ok := (<-hh.worker.all)[id]
	if ok {
		err := readPostFile(r, wo.Task.ep)
		if err != nil {
			log.WithFields(log.Fields{
				"size":  wo.Task.Size,
				"name":  wo.Task.Name,
				"host":  wo.Host,
				"start": wo.Start,
			}).Warn("アップロードに失敗しました。", err)
			return err
		}
		wo.Task.Duration = getMovieDuration(wo.Task.ep)
		if wo.Task.Duration == 0 {
			// 動画の長さがゼロはおかしい
			// ファイルを消しておく
			os.Remove(wo.Task.ep)
			log.WithFields(log.Fields{
				"size":  wo.Task.Size,
				"name":  wo.Task.Name,
				"host":  wo.Host,
				"start": wo.Start,
			}).Warn("動画の長さがゼロです。", err)
			return errors.New("動画の長さがゼロのようです")
		}
		// 削除フォルダに移動
		err = os.Rename(wo.Task.rp, wo.Task.dp)
		if err != nil {
			log.WithFields(log.Fields{
				"size":  wo.Task.Size,
				"name":  wo.Task.Name,
				"host":  wo.Host,
				"start": wo.Start,
			}).Warn("RAW動画の移動に失敗しました。", err)
			return err
		}
		wo.End = time.Now()

		// お仕事完了
		hh.worker.del <- id
		// お仕事完了リストに追加
		hh.completed.add <- wo
		log.WithFields(log.Fields{
			"size":  wo.Task.Size,
			"name":  wo.Task.Name,
			"host":  wo.Host,
			"start": wo.Start,
			"end":   wo.End,
		}).Info("お仕事が完遂されました。")
		// サムネイル作成
		hh.thumbc <- Thumbnail{
			d:  wo.Task.Duration,
			ep: wo.Task.ep,
			tp: wo.Task.tp,
		}
	} else {
		return errors.New("仕事が無い")
	}
	return nil
}

func (hh *himawariHandle) taskToWorker(r *http.Request) *Task {
	rh, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		rh = r.RemoteAddr
	}
	wo := Worker{
		Host:  rh,
		Start: time.Now(),
	}
	var t *Task
EXIT:
	for {
		select {
		case t = <-hh.tasks.pop:
			if t == nil {
				break EXIT
			}
			if isExist(t.ep) {
				// エンコード後ファイルが存在するのでスキップ
				log.WithFields(log.Fields{
					"size":     t.Size,
					"name":     t.Name,
					"raw_path": t.rp,
				}).Info("すでにエンコードされている作品のようです。")
				break
			}
			wo.Task = t
			hh.worker.add <- himawariWorkerAddItem{
				id: t.Id,
				w:  wo,
			}
			log.WithFields(log.Fields{
				"size":  wo.Task.Size,
				"name":  wo.Task.Name,
				"host":  wo.Host,
				"start": wo.Start,
			}).Info("お仕事が開始されました。")
			break EXIT
		default:
			// 次の仕事なし
			break EXIT
		}
	}
	return t
}

func (hh *himawariHandle) workerToTask(idlist ...string) {
	worker := <-hh.worker.all
	for _, id := range idlist {
		wo, ok := worker[id]
		if ok {
			// 新しいUUIDにする
			wo.Task.Id = NewUUID()
			hh.worker.del <- id
			hh.tasks.add <- wo.Task
			log.WithFields(log.Fields{
				"size":  wo.Task.Size,
				"name":  wo.Task.Name,
				"start": wo.Start,
			}).Info("仕事をタスクリストに戻しました。")
		}
	}
}

func NewUUID() string {
	return uuid.New().String()
}

func NewHimawari() *himawariHandle {
	dir, err := ioutil.ReadDir(RAW_PATH)
	if err != nil {
		log.WithError(err).Warn("RAW_PATHの読み込みに失敗しました。")
		return nil
	}

	hh := &himawariHandle{}
	hh.file = http.FileServer(http.Dir(HTTP_DIR))
	hh.tasks = func() himawariTask {
		allc := make(chan []*Task)
		popc := make(chan *Task)
		addc := make(chan *Task, 4)
		go func() {
			data := make([]*Task, 0, 16)
			var datacopy []*Task
			var popcw chan<- *Task
			datahead := func() *Task {
				if len(data) > 0 {
					return data[0]
				}
				return nil
			}
			for {
				select {
				case allc <- datacopy:
				case popcw <- datahead():
					data = data[1:]
					if len(data) <= 0 {
						popcw = nil
					}
				case t := <-addc:
					data = append(data, t)
					popcw = popc
				}
				datacopy = make([]*Task, len(data))
				copy(datacopy, data)
			}
		}()
		return himawariTask{
			all: allc,
			pop: popc,
			add: addc,
		}
	}()
	hh.worker = func() himawariWorker {
		allc := make(chan map[string]Worker)
		delc := make(chan string, 4)
		addc := make(chan himawariWorkerAddItem, 4)
		go func() {
			tic := time.NewTicker(WORKER_CHECK_DURATION)
			data := make(map[string]Worker)
			var datacopy map[string]Worker
			for {
				select {
				case allc <- datacopy:
				case id := <-delc:
					delete(data, id)
				case item := <-addc:
					data[item.id] = item.w
				case now := <-tic.C:
					idlist := []string{}
					for key, it := range data {
						if now.After(it.Start.Add(WORKER_DELETE_DURATION)) {
							idlist = append(idlist, key)
						}
					}
					// goroutine使わないとデッドロックする
					go hh.workerToTask(idlist...)
				}
				datacopy = make(map[string]Worker)
				for k, v := range data {
					datacopy[k] = v
				}
			}
		}()
		return himawariWorker{
			all: allc,
			add: addc,
			del: delc,
		}
	}()
	hh.completed = func() himawariComplete {
		allc := make(chan []Worker)
		addc := make(chan Worker)
		go func() {
			tic := time.NewTicker(WORKER_CHECK_DURATION)
			data := make([]Worker, 0, 16)
			var datacopy []Worker
			for {
				select {
				case allc <- datacopy:
				case w := <-addc:
					data = append(data, w)
				case now := <-tic.C:
					var i int
					for i = len(data) - 1; i >= 0; i-- {
						if now.After(data[i].End.Add(COMPLETED_DELETE_DURATION)) {
							break
						}
					}
					if i >= 0 {
						data = data[i+1:]
						log.WithFields(log.Fields{
							"count": i,
						}).Info("完了リストの定期清掃を実施しました。")
					}
				}
				datacopy = make([]Worker, len(data))
				copy(datacopy, data)
			}
		}()
		return himawariComplete{
			all: allc,
			add: addc,
		}
	}()

	for _, it := range dir {
		t := NewTask(it)
		if t != nil {
			hh.tasks.add <- t
		}
	}
	thumbChan := make(chan Thumbnail, 256)
	hh.thumbc = thumbChan
	go thumbnailcycle(thumbChan)
	go thumbnailstart(thumbChan)

	return hh
}

func thumbnailstart(tc chan<- Thumbnail) {
	catelist, err := ioutil.ReadDir(ENCODED_PATH)
	if err != nil {
		return
	}
	for _, cate := range catelist {
		if cate.IsDir() == false {
			continue
		}
		cate_path := filepath.Join(ENCODED_PATH, cate.Name())
		titlelist, err := ioutil.ReadDir(cate_path)
		if err != nil {
			continue
		}
		for _, title := range titlelist {
			if cate.IsDir() == false {
				continue
			}
			title_path := filepath.Join(cate_path, title.Name())
			stlist, err := ioutil.ReadDir(title_path)
			if err != nil {
				continue
			}
			for _, st := range stlist {
				if st.IsDir() {
					continue
				}
				st_path := filepath.Join(title_path, st.Name())
				tdir, subtitle := filepath.Split(st_path)
				cdir, title := filepath.Split(filepath.Dir(tdir))
				_, category := filepath.Split(filepath.Dir(cdir))
				tp := filepath.Join(THUMBNAIL_PATH, category, title, strings.TrimSuffix(subtitle, filepath.Ext(subtitle)))
				if isExist(tp) {
					continue
				}
				d := getMovieDuration(st_path)
				if d == 0 {
					continue
				}
				tc <- Thumbnail{
					d:  d,
					ep: st_path,
					tp: tp,
				}
			}
		}
	}
}

func thumbnailcycle(tc <-chan Thumbnail) {
	for t := range tc {
		if isExist(t.tp) {
			// 存在する場合はスルー
			continue
		}
		if err := os.MkdirAll(t.tp, 0755); err != nil {
			// フォルダ作成に失敗
			continue
		}
		count := t.d / THUMBNAIL_INTERVAL_DURATION
		var i time.Duration
		for i = 0; i <= count; i++ {
			err := createMovieThumbnail(t.ep, t.tp, i*THUMBNAIL_INTERVAL_DURATION)
			if err != nil {
				log.WithFields(log.Fields{
					"encoded_path":   t.ep,
					"thumbnail_path": t.tp,
					"duration":       t.d,
					"index":          int(i),
					"error":          err,
				}).Warn("サムネイルの作成に失敗しました。")
				break
			}
		}
		if i > count {
			log.WithFields(log.Fields{
				"encoded_path":   t.ep,
				"thumbnail_path": t.tp,
				"duration":       t.d,
				"count":          int(count),
			}).Info("サムネイル作成完了")
		}
	}
}

func NewTask(it os.FileInfo) *Task {
	if it.IsDir() {
		return nil
	}
	t := &Task{
		Id:   NewUUID(),
		Size: it.Size(),
		Name: it.Name(),
	}
	if t.Size == 0 {
		return nil
	}
	namearr := regFilename.FindStringSubmatch(t.Name)
	if len(namearr) < 8 {
		return nil
	}
	t.Category = namearr[2]

	// フォルダ作成
	category_path := filepath.Join(ENCODED_PATH, t.Category)
	cerr := os.MkdirAll(category_path, 0755)
	if cerr != nil {
		log.WithFields(log.Fields{
			"name":  t.Name,
			"error": cerr,
		}).Warn("カテゴリフォルダ作成に失敗しました。")
		return nil
	}

	// 同じような名前を検索
	t.Title = likeTitle(namearr[6], category_path)

	// 作品のフォルダを作る
	title_path := filepath.Join(category_path, t.Title)
	terr := os.MkdirAll(title_path, 0755)
	if terr != nil {
		log.WithFields(log.Fields{
			"name":  t.Name,
			"error": terr,
		}).Warn("作品フォルダ作成に失敗しました。")
		return nil
	}
	// 同じ名前ならエンコードを省略
	enc_name := namearr[6]
	if namearr[7] != "" {
		enc_name += "_" + namearr[7]
	}
	if namearr[8] != "" && namearr[8] != "n" {
		enc_name += "_" + namearr[8]
	}
	t.Subtitle = enc_name
	enc_name += ".mp4"
	t.rp = filepath.Join(RAW_PATH, t.Name)
	t.dp = filepath.Join(DELETE_PATH, t.Name)
	t.ep = filepath.Join(title_path, enc_name)
	t.tp = filepath.Join(THUMBNAIL_PATH, t.Category, t.Title, t.Subtitle)
	if isExist(t.ep) {
		// エンコード後ファイルが存在するのでスキップ
		log.WithFields(log.Fields{
			"size":     t.Size,
			"name":     t.Name,
			"raw_path": t.rp,
		}).Info("すでにエンコードされている作品のようです。")
		return nil
	}
	log.WithFields(log.Fields{
		"size":     t.Size,
		"name":     t.Name,
		"raw_path": t.rp,
	}).Info("新しいタスクを登録しました。")
	return t
}

func readPostFile(r *http.Request, p string) error {
	f, _, err := r.FormFile("videodata")
	if err != nil {
		return err
	}
	defer f.Close()
	wfp, err := os.Create(p)
	if err != nil {
		return err
	}
	_, err = io.Copy(wfp, f)
	wfp.Close()
	if err != nil {
		// ファイルを消しておく
		os.Remove(p)
		return err
	}
	return nil
}

func likeTitle(title, cp string) string {
	// タイトルが短い場合は省略
	strlen := len([]rune(title))
	if strlen > 3 {
		titlelist, err := ioutil.ReadDir(cp)
		if err != nil {
			return title
		}
		change := 3
		if strlen >= 15 {
			// 文字数が多い場合、許容量を増やす
			change = 5
		}
		// 最小の変化量を探す
		min := struct {
			sd    int
			title string
		}{100, title}
		for _, t := range titlelist {
			tn := t.Name()
			sd := lsd.StringDistance(title, tn)
			if sd < min.sd {
				min.sd = sd
				min.title = tn
			}
		}
		// 変化量が許容量以下なら採用
		if min.sd <= change {
			title = min.title
		}
	}
	return title
}

func isExist(filename string) bool {
	_, err := os.Stat(filename)
	return err == nil
}

func getHeaderDec(r *http.Request, key string, def int64) (ret int64) {
	h := r.Header.Get(key)
	if h != "" {
		d, err := strconv.ParseInt(h, 10, 64)
		if err == nil {
			ret = d
		} else {
			ret = def
		}
	} else {
		ret = def
	}
	return
}

func getHeaderString(r *http.Request, key string, def string) (ret string) {
	h := r.Header.Get(key)
	if h != "" {
		ret = h
	} else {
		ret = def
	}
	return
}

func getMovieDuration(p string) time.Duration {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	// ffmpegもffprobeもstderrに出力するのでffmpegを使っておく
	cmd := exec.CommandContext(ctx, "ffmpeg", "-i", p)

	var sbuf strings.Builder
	cmd.Stderr = &sbuf
	cmd.Run()
	str := sbuf.String()
	index := strings.Index(str, "Duration: ")
	if index < 0 || len(str) < index+18+5 {
		return 0
	}
	arr := strings.Split(str[index+10:index+10+8], ":")
	if len(arr) < 3 {
		return 0
	}
	var d time.Duration
	h, _ := strconv.ParseUint(arr[0], 10, 32)
	m, _ := strconv.ParseUint(arr[1], 10, 32)
	s, _ := strconv.ParseUint(arr[2], 10, 32)
	d += time.Hour * time.Duration(h)
	d += time.Minute * time.Duration(m)
	d += time.Second * time.Duration(s)
	return d
}

func createMovieThumbnail(ep, tp string, dur time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	// ffmpegもffprobeもstderrに出力するのでffmpegを使っておく
	sec := int64(dur / time.Second)
	args := []string{
		"-ss", strconv.FormatInt(sec, 10),
		"-i", ep,
		"-r", "1",
		"-vframes", "1",
		"-f", "image2",
		"-vf", "scale=320:-1",
		filepath.Join(tp, fmt.Sprintf("%06d.jpg", sec)),
	}
	return exec.CommandContext(ctx, "ffmpeg", args...).Run()
}

// https://stackoverflow.com/questions/23558425/how-do-i-get-the-local-ip-address-in-go
func externalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue // not an ipv4 address
			}
			return ip.String(), nil
		}
	}
	return "", errors.New("are you connected to the network?")
}
