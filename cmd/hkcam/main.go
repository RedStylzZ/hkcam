package main

import (
	"github.com/RedStylzZ/hkcam"
	"github.com/RedStylzZ/hkcam/api"
	"github.com/RedStylzZ/hkcam/app"
	"github.com/RedStylzZ/hkcam/ffmpeg"
	"github.com/RedStylzZ/hkcam/html"
	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	log "github.com/sirupsen/logrus"
	"github.com/unrolled/render"

	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

var (
	// Date is build date.
	Date string

	// Version is the app version.
	Version string

	// BuildMode is the build mode ("debug", "release")
	BuildMode string
)

const (
	DateLayout = "2006-01-02T15:04:05Z-0700"
)

func main() {

	// Platform dependent flags
	var inputDevice, inputFilename, loopbackFilename, h264Encoder, h264Decoder, dataDir, pin, port string
	var minVideoBitrate int
	var multiStream, verbose bool

	switch runtime.GOOS {
	case "linux":
		flag.StringVar(&inputDevice, "input_device", "v4l2", "video input device")
		flag.StringVar(&inputFilename, "input_filename", "/dev/video0", "video input device filename")
		flag.StringVar(&loopbackFilename, "loopback_filename", "/dev/video99", "video loopback device filename")
		flag.StringVar(&h264Decoder, "h264_decoder", "", "h264 video decoder")
		flag.StringVar(&h264Encoder, "h264_encoder", "h264_v4l2m2m", "h264 video encoder")
	case "darwin":
		flag.StringVar(&inputDevice, "input_device", "avfoundation", "video input device")
		flag.StringVar(&inputFilename, "input_filename", "default", "video input device filename")
		// loopback is not needed on macOS because avfoundation provides multi-access to the camera
		flag.StringVar(&loopbackFilename, "loopback_filename", "", "video loopback device filename")
		flag.StringVar(&h264Decoder, "h264_decoder", "", "h264 video decoder")
		flag.StringVar(&h264Encoder, "h264_encoder", "h264_videotoolbox", "h264 video encoder")
	default:
		log.Fatalf("%s platform is not supported", runtime.GOOS)
	}

	flag.StringVar(&dataDir, "data_dir", "db", "Path to data directory")
	flag.StringVar(&port, "port", "8080", "Port on which transport is reachable")
	flag.StringVar(&pin, "pin", "00102003", "PIN for HomeKit pairing")
	flag.BoolVar(&multiStream, "multi_stream", false, "Allow multiple clients to view the stream simultaneously")
	flag.BoolVar(&verbose, "verbose", false, "Verbose logging")
	flag.IntVar(&minVideoBitrate, "min_video_bitrate", 0, "minimum video bit rate in kbps")

	flag.Parse()

	if verbose {
		log.SetLevel(log.DebugLevel)
		ffmpeg.EnableVerboseLogging()
	}

	buildDate, err := time.Parse(DateLayout, Date)
	if err != nil {
		log.Fatal(err)
	}

	log.Infof("version %s (built at %s)", Version, Date)

	switchInfo := accessory.Info{Name: "Camera", Firmware: Version, Manufacturer: "Matthias Hochgatterer"}
	cam := accessory.NewCamera(switchInfo)

	cfg := ffmpeg.Config{
		InputDevice:      inputDevice,
		InputFilename:    inputFilename,
		LoopbackFilename: loopbackFilename,
		H264Decoder:      h264Decoder,
		H264Encoder:      h264Encoder,
		MinVideoBitrate:  minVideoBitrate,
		MultiStream:      multiStream,
	}

	ffmpeg := hkcam.SetupFFMPEGStreaming(cam, cfg)

	// Add a custom camera control service to record snapshots
	cc := hkcam.NewCameraControl()
	cam.Control.AddC(cc.Assets.C)
	cam.Control.AddC(cc.GetAsset.C)
	cam.Control.AddC(cc.DeleteAssets.C)
	cam.Control.AddC(cc.TakeSnapshot.C)

	store := hap.NewFsStore(dataDir)
	s, err := hap.NewServer(store, cam.A)
	if err != nil {
		log.Panic(err)
	}

	s.Pin = pin
	s.Addr = fmt.Sprintf(":%s", port)

	s.ServeMux().HandleFunc("/resource", func(res http.ResponseWriter, req *http.Request) {
		if !s.IsAuthorized(req) {
			hap.JsonError(res, hap.JsonStatusInsufficientPrivileges)
			return
		}

		if req.Method != http.MethodPost {
			res.WriteHeader(http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(req.Body)
		if err != nil {
			log.Info(err)
			res.WriteHeader(http.StatusInternalServerError)
			return
		}

		r := struct {
			Type   string `json:"resource-type"`
			Width  uint   `json:"image-width"`
			Height uint   `json:"image-height"`
		}{}

		err = json.Unmarshal(body, &r)
		if err != nil {
			log.Info(err)
			res.WriteHeader(http.StatusBadRequest)
			return
		}

		log.Debugf("Response: %+v", r)

		switch r.Type {
		case "image":
			b, err := createSnapshot(r.Width, r.Height, ffmpeg)
			if err != nil {
				log.Error(err)
				res.WriteHeader(http.StatusInternalServerError)
				return
			}

			res.Header().Set("Content-Type", "image/jpeg")
			_, err = hap.NewChunkedWriter(res, 2048).Write(b)
			if err != nil {
				log.Errorf("write image: %s", err)
			}
		default:
			log.Infof("unsupported resource request \"%s\"", r.Type)
			res.WriteHeader(http.StatusInternalServerError)
			return
		}
	})

	cc.SetupWithDir(dataDir)
	cc.CameraSnapshotReq = func(width, height uint) (*image.Image, error) {
		snapshot, err := ffmpeg.Snapshot(width, height)
		if err != nil {
			return nil, err
		}

		return &snapshot.Image, nil
	}

	appl := &app.App{
		BuildMode: BuildMode,
		BuildDate: buildDate,
		Version:   Version,
		Launch:    time.Now(),
		Store:     store,
		FFMPEG:    ffmpeg,
	}
	api := &api.Api{
		App: appl,
	}

	// files are served via fs.go
	// fs := &embedFS{}
	fs := render.LocalFileSystem{}

	funcs := template.FuncMap{
		"_formatDate": func(d time.Time) string { return d.Format(time.RFC3339) },
		"_safeHTML":   func(s string) template.HTML { return template.HTML(s) },
		"T":           func(s string, args ...interface{}) string { return fmt.Sprintf(s, args...) },
	}

	html := html.Html{
		Store:     store,
		BuildMode: BuildMode,
		Api:       api,
		App:       appl,
		// FileSystem: fs,
		Render: render.New(render.Options{
			Directory:  "html/tmpl",
			FileSystem: fs,
			Layout:     "layout",
			Funcs:      []template.FuncMap{funcs},
		}),
	}

	s.ServeMux().Mount("/api", api.Router())
	s.ServeMux().Mount("/", html.Router())

	// serve static files
	staticFs := http.FileServer(http.Dir("static"))
	s.ServeMux().HandleFunc("/static/*", func(w http.ResponseWriter, r *http.Request) {
		staticFs.ServeHTTP(w, r)
	})

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)
	signal.Notify(c, syscall.SIGUSR1)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-c
		signal.Stop(c) // stop delivering signals
		cancel()
	}()

	if err := s.ListenAndServe(ctx); err != nil {
		if err != ctx.Err() {
			log.Error(err)
		}
	}
}

func createSnapshot(width, height uint, ffmpeg ffmpeg.FFMPEG) ([]byte, error) {
	log.Debugf("snapshot %dw x %dh\n", width, height)

	snapshot, err := ffmpeg.Snapshot(width, height)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %v", err)
	}

	buf := new(bytes.Buffer)
	if err := jpeg.Encode(buf, snapshot.Image, nil); err != nil {
		return nil, fmt.Errorf("encode: %v", err)
	}

	return buf.Bytes(), nil
}

// Not used anymore
/* // embedFS serves files
type embedFS struct {
}

func (embedFS) Walk(root string, walkFn filepath.WalkFunc) error {
	for path, file := range _escData {
		stat, err := file.Stat()
		err = walkFn(path, stat, err)
		if err != nil {
			return err
		}
	}

	return nil
}

func (embedFS) ReadFile(filename string) ([]byte, error) {
	return FSByte(false, filename)
}
*/
