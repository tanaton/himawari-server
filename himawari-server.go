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
	"go.uber.org/zap"
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
	THUMBNAIL_INTERVAL_DURATION = 10
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

type himawariTaskAllItem struct {
	ch chan<- []*Task
}
type himawariTaskPopItem struct {
	ch chan<- *Task
}
type himawariTask struct {
	all chan<- himawariTaskAllItem
	pop chan<- himawariTaskPopItem
	add chan<- *Task
}

type himawariWorkerAllItem struct {
	ch chan<- map[string]Worker
}
type himawariWorkerAddItem struct {
	id string
	w  Worker
}
type himawariWorkerGetItem struct {
	id string
	ch chan<- Worker
}
type himawariWorker struct {
	all chan<- himawariWorkerAllItem
	add chan<- himawariWorkerAddItem
	del chan<- string
	get chan<- himawariWorkerGetItem
}

type himawariCompleteAllItem struct {
	ch chan<- []Worker
}
type himawariComplete struct {
	all chan<- himawariCompleteAllItem
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
var log *zap.SugaredLogger

func init() {
	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	log = logger.Sugar()

	serverIP, err = externalIP()
	if err != nil {
		log.Fatal("自身のIPアドレスの取得に失敗しました。", err)
	}
}

func main() {
	log.Fatal(run())
}

func run() error {
	defer log.Sync()
	h := &http.Server{
		Addr:    fmt.Sprintf(":%d", HTTP_PORT),
		Handler: NewHimawari(),
	}
	return h.ListenAndServe()
}

func (hh *himawariHandle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := path.Clean(r.URL.Path)
	if strings.Index(p, "/video/id/") == 0 {
		if r.Method == "GET" {
			wo, err := hh.worker.Get(p[10:])
			if err != nil {
				rfp, err := os.Open(wo.Task.rp)
				if err != nil {
					// そんなファイルはない
					http.NotFound(w, r)
					log.Infow("存在しないファイルです。", "path", r.URL.Path, "method", r.Method)
				} else {
					defer rfp.Close()
					http.ServeContent(w, r, wo.Task.Name, wo.Start, rfp)
				}
			} else {
				// そんな仕事はない
				http.NotFound(w, r)
				log.Infow("存在しない仕事です。", "path", r.URL.Path, "method", r.Method)
			}
		} else {
			http.Error(w, "GET以外のメソッドには対応していません。", http.StatusMethodNotAllowed)
			log.Infow("対応していないメソッドです。", "path", r.URL.Path, "method", r.Method)
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
						log.Infow("お仕事の転送に失敗しました。",
							"error", err,
							"path", r.URL.Path,
							"size", tt.Size,
							"name", tt.Name,
						)
					} else {
						log.Infow("お仕事の転送に成功しました。",
							"path", r.URL.Path,
							"send", n,
						)
					}
				} else {
					// お仕事やり直し
					hh.workerToTask(t.Id)
					http.Error(w, "お仕事のjsonエンコードに失敗しました。", http.StatusInternalServerError)
					log.Infow("お仕事のjsonエンコードに失敗しました。",
						"error", err,
						"path", r.URL.Path,
						"size", tt.Size,
						"name", tt.Name,
					)
				}
			} else {
				// お仕事はない
				http.NotFound(w, r)
				log.Infow("仕事がありません。",
					"path", r.URL.Path,
					"method", r.Method,
				)
			}
		default:
			http.Error(w, "GET、POST以外のメソッドには対応していません。", http.StatusMethodNotAllowed)
			log.Infow("対応していないメソッドです。",
				"path", r.URL.Path,
				"method", r.Method,
			)
		}
	} else if p == "/task/add" {
		// お仕事を追加する
		if r.Method == "POST" {
			stat, err := os.Stat(filepath.Join(RAW_PATH, r.PostFormValue("filename")))
			if err == nil {
				t := NewTask(stat)
				if t != nil {
					// 追加
					hh.tasks.Add(t)
				} else {
					// 失敗しても特にエラーではない
				}
				w.WriteHeader(http.StatusOK)
			} else {
				http.Error(w, "リクエストされたファイルが存在しないようです。", http.StatusBadRequest)
			}
		} else {
			http.Error(w, "POST以外のメソッドには対応していません。", http.StatusMethodNotAllowed)
			log.Infow("対応していないメソッドです。",
				"path", r.URL.Path,
				"method", r.Method,
			)
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
			log.Infow("対応していないメソッドです。",
				"path", r.URL.Path,
				"method", r.Method,
			)
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
	tall, _ := hh.tasks.All()
	wall, _ := hh.worker.All()
	call, _ := hh.completed.All()
	db := Dashboard{
		Tasks:     tall,
		Worker:    wall,
		Completed: call,
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
		log.Warn("index.htmlの生成に失敗しました。", err)
	}
}

func (hh *himawariHandle) done(r *http.Request) error {
	id := r.PostFormValue("uuid")

	wo, err := hh.worker.Get(id)
	if err != nil {
		err := readPostFile(r, wo.Task.ep)
		if err != nil {
			log.Warnw("アップロードに失敗しました。",
				"error", err,
				"size", wo.Task.Size,
				"name", wo.Task.Name,
				"host", wo.Host,
				"start", wo.Start,
			)
			return err
		}
		wo.Task.Duration = getMovieDuration(wo.Task.ep)
		if wo.Task.Duration == 0 {
			// 動画の長さがゼロはおかしい
			// ファイルを消しておく
			os.Remove(wo.Task.ep)
			log.Warnw("動画の長さがゼロです。",
				"error", err,
				"size", wo.Task.Size,
				"name", wo.Task.Name,
				"host", wo.Host,
				"start", wo.Start,
			)
			return errors.New("動画の長さがゼロのようです")
		}
		// 削除フォルダに移動
		err = os.Rename(wo.Task.rp, wo.Task.dp)
		if err != nil {
			log.Warnw("RAW動画の移動に失敗しました。",
				"error", err,
				"size", wo.Task.Size,
				"name", wo.Task.Name,
				"host", wo.Host,
				"start", wo.Start,
			)
			return err
		}
		wo.End = time.Now()

		// お仕事完了
		hh.worker.Del(id)
		// お仕事完了リストに追加
		hh.completed.Add(wo)
		log.Infow("お仕事が完遂されました。",
			"size", wo.Task.Size,
			"name", wo.Task.Name,
			"host", wo.Host,
			"start", wo.Start,
			"end", wo.End,
		)
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
	for {
		t, err = hh.tasks.Pop()
		if err != nil {
			// タスクが空
			break
		}
		if isExist(t.ep) {
			// エンコード後ファイルが存在するのでスキップ
			log.Infow("すでにエンコードされている作品のようです。",
				"size", t.Size,
				"name", t.Name,
				"raw_path", t.rp,
			)
			continue
		}
		wo.Task = t
		hh.worker.Add(t.Id, wo)
		log.Infow("お仕事が開始されました。",
			"size", wo.Task.Size,
			"name", wo.Task.Name,
			"host", wo.Host,
			"start", wo.Start,
		)
		break
	}
	return t
}

func (hh *himawariHandle) workerToTask(idlist ...string) {
	for _, id := range idlist {
		wo, err := hh.worker.Get(id)
		if err != nil {
			// 新しいUUIDにする
			wo.Task.Id = NewUUID()
			hh.worker.Del(id)
			hh.tasks.Add(wo.Task)
			log.Infow("仕事をタスクリストに戻しました。",
				"size", wo.Task.Size,
				"name", wo.Task.Name,
				"start", wo.Start,
			)
		}
	}
}

func NewUUID() string {
	return uuid.New().String()
}

func NewHimawari() *himawariHandle {
	dir, err := ioutil.ReadDir(RAW_PATH)
	if err != nil {
		log.Warn("RAW_PATHの読み込みに失敗しました。", err)
		return nil
	}

	hh := &himawariHandle{}
	hh.file = http.FileServer(http.Dir(HTTP_DIR))
	hh.tasks = func() himawariTask {
		allc := make(chan himawariTaskAllItem)
		popc := make(chan himawariTaskPopItem)
		addc := make(chan *Task, 4)
		go func() {
			data := make([]*Task, 0, 16)
			for {
				select {
				case item := <-allc:
					datacopy := make([]*Task, len(data))
					copy(datacopy, data)
					item.ch <- datacopy
				case item := <-popc:
					if len(data) > 0 {
						item.ch <- data[0]
						data = data[1:]
					} else {
						close(item.ch)
					}
				case t := <-addc:
					data = append(data, t)
				}
			}
		}()
		return himawariTask{
			all: allc,
			pop: popc,
			add: addc,
		}
	}()
	hh.worker = func() himawariWorker {
		allc := make(chan himawariWorkerAllItem)
		addc := make(chan himawariWorkerAddItem, 4)
		delc := make(chan string, 4)
		getc := make(chan himawariWorkerGetItem)
		go func() {
			tic := time.NewTicker(WORKER_CHECK_DURATION)
			data := make(map[string]Worker)
			for {
				select {
				case item := <-allc:
					datacopy := make(map[string]Worker)
					for k, v := range data {
						datacopy[k] = v
					}
					item.ch <- datacopy
				case item := <-addc:
					data[item.id] = item.w
				case id := <-delc:
					delete(data, id)
				case item := <-getc:
					w, ok := data[item.id]
					if ok {
						item.ch <- w
					} else {
						close(item.ch)
					}
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
			}
		}()
		return himawariWorker{
			all: allc,
			add: addc,
			del: delc,
			get: getc,
		}
	}()
	hh.completed = func() himawariComplete {
		allc := make(chan himawariCompleteAllItem)
		addc := make(chan Worker)
		go func() {
			tic := time.NewTicker(WORKER_CHECK_DURATION)
			data := make([]Worker, 0, 16)
			for {
				select {
				case item := <-allc:
					datacopy := make([]Worker, len(data))
					copy(datacopy, data)
					item.ch <- datacopy
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
						log.Infow("完了リストの定期清掃を実施しました。", "count", i)
					}
				}
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
			hh.tasks.Add(t)
		}
	}
	thumbChan := make(chan Thumbnail, 256)
	hh.thumbc = thumbChan
	go thumbnailcycle(thumbChan)
	go thumbnailstart(thumbChan)

	return hh
}

func (ht himawariTask) All() ([]*Task, error) {
	ch := make(chan []*Task)
	ht.all <- himawariTaskAllItem{
		ch: ch,
	}
	tl, ok := <-ch
	if !ok {
		return nil, errors.New("panic")
	}
	return tl, nil
}

func (ht himawariTask) Pop() (*Task, error) {
	ch := make(chan *Task)
	ht.pop <- himawariTaskPopItem{
		ch: ch,
	}
	t, ok := <-ch
	if !ok {
		return nil, errors.New("タスクリストが空でした。")
	}
	return t, nil
}

func (ht himawariTask) Add(t *Task) {
	ht.add <- t
}

func (hw himawariWorker) All() (map[string]Worker, error) {
	ch := make(chan map[string]Worker)
	hw.all <- himawariWorkerAllItem{
		ch: ch,
	}
	mw, ok := <-ch
	if !ok {
		return nil, errors.New("panic")
	}
	return mw, nil
}

func (hw himawariWorker) Add(id string, wo Worker) {
	hw.add <- himawariWorkerAddItem{
		id: id,
		w:  wo,
	}
}

func (hw himawariWorker) Del(id string) {
	hw.del <- id
}

func (hw himawariWorker) Get(id string) (Worker, error) {
	ch := make(chan Worker)
	hw.get <- himawariWorkerGetItem{
		id: id,
		ch: ch,
	}
	w, ok := <-ch
	if !ok {
		return w, errors.New("Workerが見つかりませんでした。")
	}
	return w, nil
}

func (hc himawariComplete) All() ([]Worker, error) {
	ch := make(chan []Worker)
	hc.all <- himawariCompleteAllItem{
		ch: ch,
	}
	wl, ok := <-ch
	if !ok {
		return nil, errors.New("panic")
	}
	return wl, nil
}

func (hc himawariComplete) Add(wo Worker) {
	hc.add <- wo
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
	sy := make(chan struct{}, 8)
	for t := range tc {
		if isExist(t.tp) {
			// 存在する場合はスルー
			continue
		}
		if err := os.MkdirAll(t.tp, 0755); err != nil {
			// フォルダ作成に失敗
			continue
		}
		count := int64(t.d / (time.Second * THUMBNAIL_INTERVAL_DURATION))
		var i int64
		for i = 0; i <= count; i++ {
			sy <- struct{}{}
			go func(t Thumbnail, i int64) {
				defer func() {
					<-sy
				}()
				err := createMovieThumbnail(t.ep, t.tp, i*THUMBNAIL_INTERVAL_DURATION)
				if err != nil {
					log.Warnw("サムネイルの作成に失敗しました。",
						"encoded_path", t.ep,
						"thumbnail_path", t.tp,
						"duration", t.d,
						"index", i,
						"error", err,
					)
				}
			}(t, i)
		}
		if i > count {
			log.Infow("サムネイル作成完了",
				"encoded_path", t.ep,
				"thumbnail_path", t.tp,
				"duration", t.d,
				"count", count,
			)
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
		log.Warnw("カテゴリフォルダ作成に失敗しました。",
			"name", t.Name,
			"error", cerr,
		)
		return nil
	}

	// 同じような名前を検索
	t.Title = likeTitle(namearr[6], category_path)

	// 作品のフォルダを作る
	title_path := filepath.Join(category_path, t.Title)
	terr := os.MkdirAll(title_path, 0755)
	if terr != nil {
		log.Warnw("作品フォルダ作成に失敗しました。",
			"name", t.Name,
			"error", terr,
		)
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
		log.Infow("すでにエンコードされている作品のようです。",
			"size", t.Size,
			"name", t.Name,
			"raw_path", t.rp,
		)
		return nil
	}
	log.Infow("新しいタスクを登録しました。",
		"size", t.Size,
		"name", t.Name,
		"raw_path", t.rp,
	)
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

func createMovieThumbnail(ep, tp string, sec int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	// ffmpegもffprobeもstderrに出力するのでffmpegを使っておく
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
