package main

import (
	"compress/gzip"
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
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/NYTimes/gziphandler"
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
refs=6
deblock=0:0`

type TaskItem struct {
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
type WorkerItem struct {
	Task  *TaskItem
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
	ch chan<- []*TaskItem
}
type himawariTaskPopItem struct {
	ch chan<- *TaskItem
}
type himawariTask struct {
	allc chan<- himawariTaskAllItem
	popc chan<- himawariTaskPopItem
	addc chan<- *TaskItem
}

type himawariWorkerAllItem struct {
	ch chan<- map[string]WorkerItem
}
type himawariWorkerAddItem struct {
	id string
	w  WorkerItem
}
type himawariWorkerGetItem struct {
	id string
	ch chan<- WorkerItem
}
type himawariWorker struct {
	allc chan<- himawariWorkerAllItem
	addc chan<- himawariWorkerAddItem
	delc chan<- string
	getc chan<- himawariWorkerGetItem
}

type himawariCompleteAllItem struct {
	ch chan<- []WorkerItem
}
type himawariComplete struct {
	allc chan<- himawariCompleteAllItem
	addc chan<- WorkerItem
}

type himawariHandle struct {
	file      http.Handler
	thumbc    chan<- Thumbnail
	tasks     *himawariTask
	worker    *himawariWorker
	completed *himawariComplete
}
type Dashboard struct {
	Tasks     []*TaskItem
	Worker    map[string]WorkerItem
	Completed []WorkerItem
}
type serverItem struct {
	s *http.Server
	f func(s *http.Server) error
}
type himawari struct {
	wg sync.WaitGroup
}
type himawariTaskStartHandle struct {
	tasks  *himawariTask
	worker *himawariWorker
}
type himawariTaskDoneHandle struct {
	thumbc    chan<- Thumbnail
	worker    *himawariWorker
	completed *himawariComplete
}
type himawariTaskDataSendHandle struct {
	worker *himawariWorker
}
type himawariTaskAddHandle struct {
	tasks *himawariTask
}
type himawariDashboardHandle struct {
	tasks     *himawariTask
	worker    *himawariWorker
	completed *himawariComplete
}

var gzipContentTypeList = []string{
	"text/html",
	"text/css",
	"text/javascript",
	"text/plain",
	"application/json",
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
	defer func() {
		if err := recover(); err != nil {
			log.Warnw("panic!!!", "error", err)
			log.Sync()
			os.Exit(1)
		}
	}()
	os.Exit(_main())
}

func _main() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		log.Sync()
		cancel()
	}()

	hi := himawari{}
	if err := hi.run(ctx); err != nil {
		return 1
	}
	return 0
}

func (hi *himawari) run(bctx context.Context) error {
	ctx, exitch := hi.startExitManageProc(bctx)

	thumbChan := make(chan Thumbnail, 256)
	tasks := hi.NewHimawariTask(ctx)
	worker := hi.NewHimawariWorker(ctx, tasks)
	completed := hi.NewHimawariCompleted(ctx)

	hi.wg.Add(1)
	go hi.thumbnailcycle(ctx, thumbChan)
	hi.wg.Add(1)
	go hi.thumbnailstart(ctx, thumbChan)

	http.Handle("/video/id/", &himawariTaskDataSendHandle{
		worker: worker,
	})
	http.Handle("/task", &himawariTaskStartHandle{
		tasks:  tasks,
		worker: worker,
	})
	http.Handle("/task/add", &himawariTaskAddHandle{
		tasks: tasks,
	})
	http.Handle("/task/done", &himawariTaskDoneHandle{
		thumbc:    thumbChan,
		worker:    worker,
		completed: completed,
	})
	// http.Handle("/task/id/")
	http.Handle("/index.html", &himawariDashboardHandle{
		tasks:     tasks,
		worker:    worker,
		completed: completed,
	})
	http.Handle("/", http.FileServer(http.Dir(HTTP_DIR)))

	err := tasks.addAll()
	if err != nil {
		exitch <- struct{}{}
		log.Infow("タスク追加に失敗しました。", "error", err)
		return hi.shutdown(ctx)
	}

	ghfunc, err := gziphandler.GzipHandlerWithOpts(gziphandler.CompressionLevel(gzip.BestSpeed), gziphandler.ContentTypes(gzipContentTypeList))
	if err != nil {
		exitch <- struct{}{}
		log.Infow("サーバーハンドラの作成に失敗しました。", "error", err)
		return hi.shutdown(ctx)
	}
	h := ghfunc(http.DefaultServeMux)

	sl := append([]serverItem{}, serverItem{
		s: &http.Server{
			Addr:    fmt.Sprintf(":%d", HTTP_PORT),
			Handler: h,
		},
		f: func(s *http.Server) error { return s.ListenAndServe() },
	})

	for _, it := range sl {
		// ローカル化
		s := it
		// WEBサーバー起動
		hi.wg.Add(1)
		go s.startServer(&hi.wg)
	}

	// シャットダウン管理
	return hi.shutdown(ctx, sl...)
}

func (srv serverItem) startServer(wg *sync.WaitGroup) {
	defer wg.Done()
	log.Infow("serverItem.startServer", "Addr", srv.s.Addr)
	// サーバ起動
	err := srv.f(srv.s)
	// サーバが終了した場合
	if err != nil {
		if err == http.ErrServerClosed {
			log.Infow("サーバーがシャットダウンしました。", "error", err, "Addr", srv.s.Addr)
		} else {
			log.Warnw("サーバーが落ちました。", "error", err)
		}
	}
}

func (hi *himawari) shutdown(ctx context.Context, sl ...serverItem) error {
	// シグナル等でサーバを中断する
	<-ctx.Done()
	// シャットダウン処理用コンテキストの用意
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	for _, srv := range sl {
		hi.wg.Add(1)
		go func(ctx context.Context, srv *http.Server) {
			sctx, sscancel := context.WithTimeout(ctx, time.Second*10)
			defer func() {
				sscancel()
				hi.wg.Done()
			}()
			err := srv.Shutdown(sctx)
			if err != nil {
				log.Warnw("サーバーの終了に失敗しました。", "error", err)
			} else {
				log.Infow("サーバーの終了に成功しました。", "Addr", srv.Addr)
			}
		}(sctx, srv.s)
	}
	// サーバーの終了待機
	hi.wg.Wait()
	log.Infow("シャットダウン完了")
	return log.Sync()
}

func (hi *himawari) startExitManageProc(ctx context.Context) (context.Context, chan<- struct{}) {
	exitch := make(chan struct{}, 1)
	ectx, cancel := context.WithCancel(ctx)
	hi.wg.Add(1)
	go func(ctx context.Context, ch <-chan struct{}) {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig,
			syscall.SIGHUP,
			syscall.SIGINT,
			syscall.SIGTERM,
			syscall.SIGQUIT,
			os.Interrupt,
			os.Kill,
		)
		defer func() {
			signal.Stop(sig)
			cancel()
			hi.wg.Done()
		}()

		select {
		case <-ctx.Done():
			log.Infow("Cancel from parent")
		case s := <-sig:
			log.Infow("Signal!!", "signal", s)
		case <-ch:
			log.Infow("Exit command!!")
		}
	}(ectx, exitch)
	return ectx, exitch
}

func (hh *himawariTaskStartHandle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// お仕事
	switch r.Method {
	case "GET":
		// お仕事を得る
		t := hh.tasks.toWorker(hh.worker, r)
		if t != nil {
			tt := struct {
				TaskItem
				PresetData string
				Command    string
				Args       []string
			}{
				TaskItem:   *t,
				PresetData: PRESET_DATA,
				Command:    "ffmpeg",
				Args: []string{
					"-y",
					"-i", fmt.Sprintf("http://%s:%d/video/id/%s", serverIP, HTTP_PORT, t.Id),
					"-threads", strconv.FormatInt(getHeaderDec(r, "X-himawari-Threads", ENCODE_THREADS), 10),
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
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusOK)
			err := json.NewEncoder(w).Encode(&tt)
			if err != nil {
				// お仕事やり直し
				hh.worker.toTask(hh.tasks, t.Id)
				log.Infow("お仕事の転送に失敗しました。",
					"error", err,
					"id", t.Id,
					"path", r.URL.Path,
					"size", tt.Size,
					"name", tt.Name,
				)
			} else {
				log.Infow("お仕事の転送に成功しました。",
					"id", t.Id,
					"path", r.URL.Path,
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
		http.Error(w, "GET以外のメソッドには対応していません。", http.StatusMethodNotAllowed)
		log.Infow("対応していないメソッドです。",
			"path", r.URL.Path,
			"method", r.Method,
		)
	}
}
func (hh *himawariTaskDoneHandle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
}
func (hh *himawariTaskDataSendHandle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := path.Clean(r.URL.Path)
	if strings.Index(p, "/video/id/") == 0 {
		if r.Method == "GET" {
			wo, err := hh.worker.get(p[10:])
			if err == nil {
				rfp, err := os.Open(wo.Task.rp)
				if err == nil {
					defer rfp.Close()
					http.ServeContent(w, r, wo.Task.Name, wo.Start, rfp)
				} else {
					// そんなファイルはない
					http.NotFound(w, r)
					log.Infow("存在しないファイルです。", "path", r.URL.Path, "method", r.Method)
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
	}
}
func (hh *himawariTaskAddHandle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// お仕事を追加する
	if r.Method == "POST" {
		stat, err := os.Stat(filepath.Join(RAW_PATH, r.PostFormValue("filename")))
		if err == nil {
			t := newTask(stat)
			if t != nil {
				// 追加
				hh.tasks.add(t)
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
}
func (hh *himawariDashboardHandle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// トップページの表示
	tall, _ := hh.tasks.all()
	wall, _ := hh.worker.all()
	call, _ := hh.completed.all()
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

func (hh *himawariTaskDoneHandle) done(r *http.Request) error {
	id := r.PostFormValue("uuid")

	wo, err := hh.worker.get(id)
	if err != nil {
		return errors.New("仕事が無い")
	}
	err = readPostFile(r, wo.Task.ep)
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
	hh.worker.del(id)
	// お仕事完了リストに追加
	hh.completed.add(wo)
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
	return nil
}

func (ht *himawariTask) addAll() error {
	dir, err := ioutil.ReadDir(RAW_PATH)
	if err != nil {
		return err
	}
	// タスク生成
	for _, it := range dir {
		t := newTask(it)
		if t != nil {
			ht.add(t)
		}
	}
	return nil
}

func (ht *himawariTask) toWorker(worker *himawariWorker, r *http.Request) *TaskItem {
	rh, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		rh = r.RemoteAddr
	}
	var t *TaskItem
	for {
		t, err = ht.pop()
		if err != nil {
			// タスクが空
			break
		}
		if isExist(t.ep) {
			// エンコード後ファイルが存在するのでスキップ
			log.Infow("すでにエンコードされている作品のようです。",
				"id", t.Id,
				"size", t.Size,
				"name", t.Name,
				"raw_path", t.rp,
			)
			continue
		}
		wo := WorkerItem{
			Host:  rh,
			Start: time.Now(),
		}
		wo.Task = t
		worker.add(t.Id, wo)
		log.Infow("お仕事が開始されました。",
			"id", t.Id,
			"size", wo.Task.Size,
			"name", wo.Task.Name,
			"host", wo.Host,
			"start", wo.Start,
		)
		break
	}
	return t
}

func (hw *himawariWorker) toTask(tasks *himawariTask, idlist ...string) {
	for _, id := range idlist {
		wo, err := hw.get(id)
		if err != nil {
			continue
		}
		// 新しいUUIDにする
		wo.Task.Id = newUUID()
		hw.del(id)
		tasks.add(wo.Task)
		log.Infow("仕事をタスクリストに戻しました。",
			"size", wo.Task.Size,
			"name", wo.Task.Name,
			"start", wo.Start,
		)
	}
}

func newUUID() string {
	return uuid.New().String()
}

func (hi *himawari) NewHimawariTask(ctx context.Context) *himawariTask {
	allc := make(chan himawariTaskAllItem)
	popc := make(chan himawariTaskPopItem)
	addc := make(chan *TaskItem, 4)
	ht := &himawariTask{
		allc: allc,
		popc: popc,
		addc: addc,
	}
	hi.wg.Add(1)
	go func() {
		defer hi.wg.Done()
		data := make([]*TaskItem, 0, 16)
		for {
			select {
			case <-ctx.Done():
				log.Infow("HimawariTask終了")
				return
			case item := <-allc:
				datacopy := make([]*TaskItem, len(data))
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
	return ht
}

func (hi *himawari) NewHimawariWorker(ctx context.Context, tasks *himawariTask) *himawariWorker {
	allc := make(chan himawariWorkerAllItem)
	addc := make(chan himawariWorkerAddItem, 4)
	delc := make(chan string, 4)
	getc := make(chan himawariWorkerGetItem)
	hw := &himawariWorker{
		allc: allc,
		addc: addc,
		delc: delc,
		getc: getc,
	}
	hi.wg.Add(1)
	go func() {
		defer hi.wg.Done()
		tic := time.NewTicker(WORKER_CHECK_DURATION)
		data := make(map[string]WorkerItem)
		for {
			select {
			case <-ctx.Done():
				log.Infow("HimawariWorker終了")
				return
			case item := <-allc:
				datacopy := make(map[string]WorkerItem)
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
				hi.wg.Add(1)
				go func(idlist []string) {
					defer hi.wg.Done()
					hw.toTask(tasks, idlist...)
				}(idlist)
			}
		}
	}()
	return hw
}

func (hi *himawari) NewHimawariCompleted(ctx context.Context) *himawariComplete {
	allc := make(chan himawariCompleteAllItem)
	addc := make(chan WorkerItem)
	hc := &himawariComplete{
		allc: allc,
		addc: addc,
	}
	hi.wg.Add(1)
	go func() {
		defer hi.wg.Done()
		tic := time.NewTicker(WORKER_CHECK_DURATION)
		data := make([]WorkerItem, 0, 16)
		for {
			select {
			case <-ctx.Done():
				log.Infow("HimawariCompleted終了")
				return
			case item := <-allc:
				datacopy := make([]WorkerItem, len(data))
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
	return hc
}

func (ht himawariTask) all() ([]*TaskItem, error) {
	ch := make(chan []*TaskItem)
	ht.allc <- himawariTaskAllItem{
		ch: ch,
	}
	tl, ok := <-ch
	if !ok {
		return nil, errors.New("panic")
	}
	return tl, nil
}

func (ht himawariTask) pop() (*TaskItem, error) {
	ch := make(chan *TaskItem)
	ht.popc <- himawariTaskPopItem{
		ch: ch,
	}
	t, ok := <-ch
	if !ok {
		return nil, errors.New("タスクリストが空でした。")
	}
	return t, nil
}

func (ht himawariTask) add(t *TaskItem) {
	ht.addc <- t
}

func (hw himawariWorker) all() (map[string]WorkerItem, error) {
	ch := make(chan map[string]WorkerItem)
	hw.allc <- himawariWorkerAllItem{
		ch: ch,
	}
	mw, ok := <-ch
	if !ok {
		return nil, errors.New("panic")
	}
	return mw, nil
}

func (hw himawariWorker) add(id string, wo WorkerItem) {
	hw.addc <- himawariWorkerAddItem{
		id: id,
		w:  wo,
	}
}

func (hw himawariWorker) del(id string) {
	hw.delc <- id
}

func (hw himawariWorker) get(id string) (WorkerItem, error) {
	ch := make(chan WorkerItem)
	hw.getc <- himawariWorkerGetItem{
		id: id,
		ch: ch,
	}
	w, ok := <-ch
	if !ok {
		return w, errors.New("Workerが見つかりませんでした。")
	}
	return w, nil
}

func (hc himawariComplete) all() ([]WorkerItem, error) {
	ch := make(chan []WorkerItem)
	hc.allc <- himawariCompleteAllItem{
		ch: ch,
	}
	wl, ok := <-ch
	if !ok {
		return nil, errors.New("panic")
	}
	return wl, nil
}

func (hc himawariComplete) add(wo WorkerItem) {
	hc.addc <- wo
}

func (hi *himawari) thumbnailstart(ctx context.Context, tc chan<- Thumbnail) {
	defer hi.wg.Done()
	catelist, err := ioutil.ReadDir(ENCODED_PATH)
	if err != nil {
		return
	}
	for _, cate := range catelist {
		if cate.IsDir() == false {
			continue
		}
		catepath := filepath.Join(ENCODED_PATH, cate.Name())
		titlelist, err := ioutil.ReadDir(catepath)
		if err != nil {
			continue
		}
		for _, title := range titlelist {
			if cate.IsDir() == false {
				continue
			}
			titlepath := filepath.Join(catepath, title.Name())
			stlist, err := ioutil.ReadDir(titlepath)
			if err != nil {
				continue
			}
			for _, st := range stlist {
				if st.IsDir() {
					continue
				}
				stpath := filepath.Join(titlepath, st.Name())
				tdir, subtitle := filepath.Split(stpath)
				cdir, title := filepath.Split(filepath.Dir(tdir))
				_, category := filepath.Split(filepath.Dir(cdir))
				tp := filepath.Join(THUMBNAIL_PATH, category, title, strings.TrimSuffix(subtitle, filepath.Ext(subtitle)))
				if isExist(tp) {
					continue
				}
				d := getMovieDuration(stpath)
				if d == 0 {
					continue
				}
				select {
				case <-ctx.Done():
					log.Infow("thumbnailstart終了")
					return
				case tc <- Thumbnail{d: d, ep: stpath, tp: tp}:
				}
			}
		}
	}
	log.Infow("サムネイルのまとめ作成完了")
}

func (hi *himawari) thumbnailcycle(ctx context.Context, tc <-chan Thumbnail) {
	defer hi.wg.Done()
	sy := make(chan struct{}, 8)
	for {
		select {
		case <-ctx.Done():
			log.Infow("thumbnailcycle終了")
			return
		case t := <-tc:
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
				hi.wg.Add(1)
				go func(t Thumbnail, i int64) {
					defer func() {
						<-sy
						hi.wg.Done()
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
}

func newTask(it os.FileInfo) *TaskItem {
	if it.IsDir() {
		return nil
	}
	t := &TaskItem{
		Id:   newUUID(),
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
	cp := filepath.Join(ENCODED_PATH, t.Category)
	cerr := os.MkdirAll(cp, 0755)
	if cerr != nil {
		log.Warnw("カテゴリフォルダ作成に失敗しました。",
			"name", t.Name,
			"error", cerr,
		)
		return nil
	}

	// 同じような名前を検索
	t.Title = likeTitle(namearr[6], cp)

	// 作品のフォルダを作る
	tp := filepath.Join(cp, t.Title)
	terr := os.MkdirAll(tp, 0755)
	if terr != nil {
		log.Warnw("作品フォルダ作成に失敗しました。",
			"name", t.Name,
			"error", terr,
		)
		return nil
	}
	// 同じ名前ならエンコードを省略
	ename := namearr[6]
	if namearr[7] != "" {
		ename += "_" + namearr[7]
	}
	if namearr[8] != "" && namearr[8] != "n" {
		ename += "_" + namearr[8]
	}
	t.Subtitle = ename
	ename += ".mp4"
	t.rp = filepath.Join(RAW_PATH, t.Name)
	t.dp = filepath.Join(DELETE_PATH, t.Name)
	t.ep = filepath.Join(tp, ename)
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
