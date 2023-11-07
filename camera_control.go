package hkcam

import (
	"github.com/nfnt/resize"
	"github.com/radovskyb/watcher"
	log "github.com/sirupsen/logrus"

	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const TypeCameraControl = "19BDAD9E-6102-48D5-B413-3F11253706AE"

// RefDate represents the reference date used to generate asset ids.
// Short ids are preferred and therefore we use 1st April 2019 as the reference date.
var RefDate = time.Date(2019, 4, 1, 0, 0, 0, 0, time.UTC)

type CameraControl struct {
	TakeSnapshot *TakeSnapshot
	Assets       *Assets
	GetAsset     *GetAsset
	DeleteAssets *DeleteAssets

	CameraSnapshotReq func(width, height uint) (*image.Image, error)

	snapshots []*snapshot
	w         *watcher.Watcher
}

func NewCameraControl() *CameraControl {
	cc := CameraControl{}

	cc.TakeSnapshot = NewTakeSnapshot()
	cc.Assets = NewAssets()
	cc.GetAsset = NewGetAsset()
	cc.DeleteAssets = NewDeleteAssets()

	return &cc
}

func (cc *CameraControl) SetupWithDir(dir string) {
	r := regexp.MustCompile(`.*\.jpg`)

	fs, err := os.ReadDir(dir)
	if err != nil {
		log.Infoln(err)
	}

	for _, f := range fs {
		if !r.MatchString(f.Name()) {
			continue
		}

		info, err := f.Info()
		if err != nil {
			log.Infoln(f, err)
		}

		path := filepath.Join(dir, f.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			log.Infoln(f, err)
			continue
		}

		s := snapshot{
			ID:    f.Name(),
			Date:  info.ModTime().Format(time.RFC3339),
			Bytes: b,
			Path:  path,
		}
		cc.add(&s)

	}
	cc.updateAssetsCharacteristic()

	go cc.watch(dir, r)

	cc.GetAsset.OnValueRemoteUpdate(func(buf []byte) {
		var req GetAssetRequest
		err := json.Unmarshal(buf, &req)
		if err != nil {
			log.Fatalf("GetAssetRequest:", err)
		}

		for _, s := range cc.snapshots {
			if s.ID == req.ID {
				r := bytes.NewReader(s.Bytes)
				img, err := jpeg.Decode(r)
				if err != nil {
					log.Infof("jpeg.Decode() %v", err)
					cc.GetAsset.SetValue([]byte{})
					return
				}

				scaled := resize.Resize(req.Width, req.Height, img, resize.Lanczos3)
				imgBuf := new(bytes.Buffer)
				if err := jpeg.Encode(imgBuf, scaled, nil); err != nil {
					log.Infof("jpeg.Encode() %v", err)
					cc.GetAsset.SetValue([]byte{})
					return
				}

				cc.GetAsset.SetValue(imgBuf.Bytes())
				return
			}
		}
	})

	cc.DeleteAssets.OnValueRemoteUpdate(func(buf []byte) {
		var req DeleteAssetsRequest
		err := json.Unmarshal(buf, &req)
		if err != nil {
			log.Fatalf("GetAssetRequest: %s", err)
			return
		}

		for _, id := range req.IDs {
			err = cc.deleteWithID(id)
			if err != nil {
				log.Debugln("delete:", err)
			}
		}
	})

	cc.TakeSnapshot.OnValueRemoteUpdate(func(v bool) {
		if v {
			img, err := cc.CameraSnapshotReq(1920, 1080)
			if err != nil {
				log.Infoln(err)
			} else {
				name := fmt.Sprintf("%.0f.jpg", time.Since(RefDate).Seconds())
				path := filepath.Join(dir, name)

				buf := new(bytes.Buffer)
				if err := jpeg.Encode(buf, *img, nil); err != nil {
					log.Debugf("jpeg.Encode() %v", err)
				} else {
					os.WriteFile(path, buf.Bytes(), os.ModePerm)
				}
			}

			// Disable shutter after some timeout
			go func() {
				<-time.After(1 * time.Second)
				cc.TakeSnapshot.SetValue(false)
			}()
		}
	})
}

func (cc *CameraControl) add(s *snapshot) {
	log.Debugln("add:", s.ID)
	cc.snapshots = append(cc.snapshots, s)
}

func (cc *CameraControl) deleteWithID(id string) error {
	log.Debugln("del:", id)
	for _, s := range cc.snapshots {
		if s.ID == id {
			return os.Remove(s.Path)
		}
	}

	return fmt.Errorf("file with id %s not found", id)
}

func (cc *CameraControl) removeWithID(id string) {
	log.Debugln("rmv:", id)
	for i, s := range cc.snapshots {
		if s.ID == id {
			cc.snapshots = append(cc.snapshots[:i], cc.snapshots[i+1:]...)
			return
		}
	}
}

func (cc *CameraControl) updateAssetsCharacteristic() {
	assets := []CameraAssetMetadata{}
	for _, s := range cc.snapshots {
		asset := CameraAssetMetadata{
			ID:   s.ID,
			Date: s.Date,
		}
		assets = append(assets, asset)
	}

	p := AssetsMetadataResponse{
		Assets: assets,
	}
	if b, err := json.Marshal(p); err != nil {
		log.Infoln(err)
	} else {
		log.Debugln(string(b))
		cc.Assets.SetValue(b)
	}
}

func (cc *CameraControl) watch(dir string, r *regexp.Regexp) {
	w := watcher.New()
	w.FilterOps(watcher.Create, watcher.Remove)
	w.AddFilterHook(watcher.RegexFilterHook(r, false))

	go func() {
		for {
			select {
			case event := <-w.Event:
				switch event.Op {
				case watcher.Create:
					b, err := os.ReadFile(event.Path)
					if err != nil {
						log.Infoln(event.Path, err)
					} else {
						s := snapshot{
							ID:    event.Name(),
							Date:  event.ModTime().Format(time.RFC3339),
							Bytes: b,
							Path:  event.Path,
						}
						cc.add(&s)
					}
				case watcher.Remove:
					cc.removeWithID(event.Name())
				default:
					break
				}

				cc.updateAssetsCharacteristic()

			case err := <-w.Error:
				log.Fatal(err)
			case <-w.Closed:
				return
			}
		}
	}()

	if err := w.Add(dir); err != nil {
		log.Fatal(err)
	}

	if err := w.Start(time.Second * 1); err != nil {
		log.Fatal(err)
	}
}

type snapshot struct {
	ID    string
	Date  string
	Bytes []byte
	Path  string
}
