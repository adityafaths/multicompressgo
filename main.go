// Go port of the Streamlit app: "Multi-ZIP → JPG & Kompres 168–174 KB"
package main

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	fitz "github.com/gen2brain/go-fitz"
)

// ===== Settings (default mirrors Streamlit app) =====
var (
	SPEED_PRESET      = "fast" // or "balanced"
	MIN_SIDE_PX       = 256
	SCALE_MIN         = 0.35
	UPSCALE_MAX       = 2.0
	SHARPEN_ON_RESIZE = true
	SHARPEN_AMOUNT    = 1.0
	PDF_DPI_FAST      = 150
	PDF_DPI_BALANCED  = 200
	MASTER_ZIP_NAME   = "compressed.zip"
	MAX_QUALITY       = 95
	MIN_QUALITY       = 15
	THREADS           = 4
	TARGET_KB         = 174
	MIN_KB            = 168
	IMG_EXT           = map[string]bool{".jpg": true, ".jpeg": true, ".jfif": true, ".png": true, ".webp": true, ".tif": true, ".tiff": true, ".bmp": true, ".gif": true, ".heic": true, ".heif": true}
	PDF_EXT           = map[string]bool{".pdf": true}
	ALLOW_ZIP         = true
)

// ===== Utility functions =====
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func extLower(name string) string {
	return strings.ToLower(filepath.Ext(name))
}

// decodeImageFromBytes tries to decode JPEG/PNG/GIF/BMP/TIFF/WEBP via imaging
func decodeImageFromBytes(name string, b []byte) (image.Image, error) {
	ext := extLower(name)
	if ext == ".heic" || ext == ".heif" {
		return nil, nil
	}
	img, err := imaging.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return img, nil
}

func saveJPGBytes(img image.Image, quality int, speedFast bool) ([]byte, error) {
	buf := &bytes.Buffer{}
	opt := &jpeg.Options{Quality: quality}
	if err := jpeg.Encode(buf, img, opt); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// tryQualityBS: binary search over quality to get <= target_kb
func tryQualityBS(img image.Image, targetKB int, qmin, qmax int, speedFast bool) ([]byte, int, error) {
	lo, hi := qmin, qmax
	var best []byte
	var bestQ int

	for lo <= hi {
		mid := (lo + hi) / 2
		b, err := saveJPGBytes(img, mid, speedFast)
		if err != nil {
			return nil, 0, err
		}
		if len(b) <= targetKB*1024 {
			best, bestQ = b, mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best == nil {
		return nil, 0, nil
	}
	return best, bestQ, nil
}

func resizeToScale(img image.Image, scale float64, doSharpen bool, amount float64) image.Image {
	w := int(float64(img.Bounds().Dx()) * scale)
	h := int(float64(img.Bounds().Dy()) * scale)
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	out := imaging.Resize(img, w, h, imaging.Lanczos)
	if doSharpen && amount > 0 {
		out = imaging.Sharpen(out, amount)
	}
	return out
}

func ensureMinSide(img image.Image, minSide int, doSharpen bool, amount float64) image.Image {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()
	if w >= minSide && h >= minSide {
		return img
	}
	scale := float64(minSide) / float64(min(w, h))
	if scale < 1.0 {
		scale = 1.0
	}
	return resizeToScale(img, scale, doSharpen, amount)
}

// compressIntoRange attempts to produce JPEG in [min_kb, max_kb]
func compressIntoRange(baseImg image.Image, minKB, maxKB, minSide int, scaleMin, upscaleMax float64, doSharpen bool, sharpenAmount float64, speedFast bool) ([]byte, float64, int, int, error) {
	// convert to opaque white background if needed
	// create RGB with white bg
	rgb := imaging.New(baseImg.Bounds().Dx(), baseImg.Bounds().Dy(), color.White)
	draw.Draw(rgb, rgb.Bounds(), baseImg, baseImg.Bounds().Min, draw.Over)

	// try quality on original size first
	data, q, err := tryQualityBS(rgb, maxKB, MIN_QUALITY, MAX_QUALITY, speedFast)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	if data != nil {
		return data, 1.0, q, len(data), nil
	}

	// binary search over scale between scaleMin..1.0
	lo, hi := scaleMin, 1.0
	var bestData []byte
	var bestScale float64
	var bestQ int
	maxSteps := 8
	if !speedFast {
		maxSteps = 12
	}

	for i := 0; i < maxSteps; i++ {
		mid := (lo + hi) / 2
		candidate := resizeToScale(rgb, mid, doSharpen, sharpenAmount)
		candidate = ensureMinSide(candidate, minSide, doSharpen, sharpenAmount)
		d, q2, err := tryQualityBS(candidate, maxKB, MIN_QUALITY, MAX_QUALITY, speedFast)
		if err != nil {
			return nil, 0, 0, 0, err
		}
		if d != nil {
			bestData, bestScale, bestQ = d, mid, q2
			lo = mid + (hi-mid)*0.35
		} else {
			hi = mid - (mid-lo)*0.35
		}
		if hi-lo < 1e-3 {
			break
		}
	}

	if bestData == nil {
		// fall back: smallest at scaleMin
		small := resizeToScale(rgb, scaleMin, doSharpen, sharpenAmount)
		small = ensureMinSide(small, minSide, doSharpen, sharpenAmount)
		d, _ := saveJPGBytes(small, MIN_QUALITY, speedFast)
		return d, scaleMin, MIN_QUALITY, len(d), nil
	}

	// if size < minKB, try upscales
	sizeB := len(bestData)
	curScale := bestScale
	if sizeB < minKB*1024 {
		imgNow := resizeToScale(rgb, curScale, doSharpen, sharpenAmount)
		imgNow = ensureMinSide(imgNow, minSide, doSharpen, sharpenAmount)
		d, q2, err := tryQualityBS(imgNow, maxKB, max(bestQ, MIN_QUALITY), MAX_QUALITY, speedFast)
		if err == nil && d != nil && len(d) > sizeB {
			bestData, bestQ, sizeB = d, q2, len(d)
		}

		iters := 0
		maxIters := 6
		if !speedFast {
			maxIters = 12
		}
		for sizeB < minKB*1024 && curScale < upscaleMax && iters < maxIters {
			curScale = curScale * 1.2
			if curScale > upscaleMax {
				curScale = upscaleMax
			}
			candidate := resizeToScale(rgb, curScale, doSharpen, sharpenAmount)
			candidate = ensureMinSide(candidate, minSide, doSharpen, sharpenAmount)
			d, q3, err := tryQualityBS(candidate, maxKB, MIN_QUALITY, MAX_QUALITY, speedFast)
			if err != nil {
				iters++
				continue
			}
			if d == nil {
				curScale *= 0.95
				iters++
				continue
			}
			if len(d) > sizeB {
				bestData, bestQ, sizeB, bestScale = d, q3, len(d), curScale
			}
			iters++
		}
	}
	return bestData, bestScale, bestQ, len(bestData), nil
}

// ----- PDF to images using go-fitz -----
func pdfBytesToImages(pdfBytes []byte, dpi int) ([]image.Image, error) {
	// go-fitz requires a filename on disk, write to temp file
	tmp, err := os.CreateTemp("", "upload-*.pdf")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(pdfBytes); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()

	doc, err := fitz.New(tmp.Name())
	if err != nil {
		return nil, err
	}
	defer doc.Close()

	imgs := []image.Image{}
	for n := 0; n < doc.NumPage(); n++ {
		page, err := doc.Image(n)
		if err != nil {
			return nil, err
		}
		imgs = append(imgs, page)
	}
	return imgs, nil
}

// ----- ZIP extraction -----
func extractZipToMemory(b []byte) ([]struct {
	Rel  string
	Data []byte
}, error) {
	r := bytes.NewReader(b)
	zf, err := zip.NewReader(r, int64(len(b)))
	if err != nil {
		return nil, err
	}
	out := []struct {
		Rel  string
		Data []byte
	}{}
	for _, f := range zf.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			continue
		}
		out = append(out, struct {
			Rel  string
			Data []byte
		}{Rel: f.Name, Data: data})
	}
	return out, nil
}

// ----- Processing one file entry -----
func processOneFileEntry(relpath string, raw []byte, label string, cfg map[string]string) (string, []string, []string, map[string][]byte) {
	processed := []string{}
	skipped := []string{}
	outs := map[string][]byte{}
	ext := strings.ToLower(filepath.Ext(relpath))
	speedFast := cfg["speed"] == "fast"
	minSide, _ := strconv.Atoi(cfg["min_side"])
	scaleMin, _ := strconv.ParseFloat(cfg["scale_min"], 64)
	upscaleMax, _ := strconv.ParseFloat(cfg["upscale_max"], 64)
	doSharpen := cfg["sharpen"] == "1"
	shAmount, _ := strconv.ParseFloat(cfg["sharpen_amount"], 64)
	pdfdpi := PDF_DPI_FAST
	if !speedFast {
		pdfdpi = PDF_DPI_BALANCED
	}

	defer func() {
		if r := recover(); r != nil {
			skipped = append(skipped, fmt.Sprintf("panic: %v", r))
		}
	}()

	if PDF_EXT[ext] {
		images, err := pdfBytesToImages(raw, pdfdpi)
		if err != nil {
			skipped = append(skipped, relpath+": pdf render error: "+err.Error())
			return label, processed, skipped, outs
		}
		for idx, img := range images {
			data, scale, q, sizeB, err := compressIntoRange(img, MIN_KB, TARGET_KB, minSide, scaleMin, upscaleMax, doSharpen, shAmount, speedFast)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s (page %d): %v", relpath, idx+1, err))
				continue
			}
			outRel := strings.TrimSuffix(relpath, filepath.Ext(relpath)) + fmt.Sprintf("_p%d.jpg", idx+1)
			outs[outRel] = data
			processed = append(processed, fmt.Sprintf("%s -> %d bytes scale=%.3f q=%d", outRel, sizeB, scale, q))
		}
	} else if IMG_EXT[ext] {
		if ext == ".heic" || ext == ".heif" {
			skipped = append(skipped, relpath+": Butuh HEIC decoder (tidak tersedia)")
			return label, processed, skipped, outs
		}
		img, err := decodeImageFromBytes(relpath, raw)
		if err != nil {
			skipped = append(skipped, relpath+": decode error: "+err.Error())
			return label, processed, skipped, outs
		}
		if img == nil {
			skipped = append(skipped, relpath+": decode returned nil")
			return label, processed, skipped, outs
		}
		if ext == ".gif" {
			// keep first frame
			// imaging.Decode already decodes first frame for GIF
		}
		data, scale, q, sizeB, err := compressIntoRange(img, MIN_KB, TARGET_KB, minSide, scaleMin, upscaleMax, doSharpen, shAmount, speedFast)
		if err != nil {
			skipped = append(skipped, relpath+": compress error: "+err.Error())
			return label, processed, skipped, outs
		}
		outRel := strings.TrimSuffix(relpath, filepath.Ext(relpath)) + ".jpg"
		outs[outRel] = data
		processed = append(processed, fmt.Sprintf("%s -> %d bytes scale=%.3f q=%d", outRel, sizeB, scale, q))
	}
	return label, processed, skipped, outs
}

// ===== HTTP Handlers & server =====
// For simplicity we store generated zips in memory keyed by token
var memZips = struct {
	sync.RWMutex
	m map[string][]byte
}{m: map[string][]byte{}}

// ===== Templates =====
var tplIndex = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="id">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width,initial-scale=1" />
  <title>Multi-ZIP → JPG & Kompres 168–174 KB</title>
  <link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.0/dist/css/bootstrap.min.css" rel="stylesheet">
</head>
<body class="bg-light">
  <div class="container-fluid py-4">
    <div class="row">
      <div class="col-md-3">
        <div class="card mb-3">
          <div class="card-body">
            <h5 class="card-title">⚙️ Pengaturan</h5>
            <form method="post" action="/process" enctype="multipart/form-data">
              <div class="mb-2">
                <label class="form-label">Preset kecepatan</label>
                <select name="speed" class="form-select">
                  <option value="fast" selected>fast</option>
                  <option value="balanced">balanced</option>
                </select>
              </div>
              <div class="mb-2">
                <label class="form-label">Sisi terpendek minimum (px)</label>
                <input name="min_side" type="number" class="form-control" value="256" min="64" max="2048" step="32">
              </div>
              <div class="mb-2">
                <label class="form-label">Skala minimum saat downscale</label>
                <input name="scale_min" type="number" class="form-control" step="0.01" value="0.35">
              </div>
              <div class="mb-2">
                <label class="form-label">Batas upscale maksimum</label>
                <input name="upscale_max" type="number" class="form-control" step="0.1" value="2.0">
              </div>
              <div class="form-check mb-2">
                <input class="form-check-input" type="checkbox" name="sharpen" id="sharpen" checked>
                <label class="form-check-label" for="sharpen">Sharpen ringan setelah resize</label>
              </div>
              <div class="mb-2">
                <label class="form-label">Sharpen amount</label>
                <input name="sharpen_amount" type="number" class="form-control" step="0.1" value="1.0">
              </div>
              <div class="mb-2">
                <label class="form-label">Nama master ZIP</label>
                <input name="master_name" class="form-control" value="compressed.zip">
              </div>
              <p><small class="text-muted">Target otomatis: 168–174 KB (tidak bisa diubah)</small></p>
              <hr>
              <div class="mb-3">
                <label class="form-label">Upload (ZIP / gambar / PDF)</label>
                <input class="form-control" type="file" name="files" multiple>
              </div>
              <button class="btn btn-primary" type="submit">🚀 Proses & Buat Master ZIP</button>
            </form>
          </div>
        </div>
        <div class="card">
          <div class="card-body">
            <h6>Catatan</h6>
            <ul>
              <li>Video tidak diterima.</li>
              <li>HEIC/HEIF: belum didukung—akan dilewati.</li>
              <li>PDF membutuhkan MuPDF di sistem (go-fitz).</li>
            </ul>
          </div>
        </div>
      </div>
      <div class="col-md-9">
        <div class="card">
          <div class="card-body">
            <h3>📦 Multi-ZIP / Files → JPG & Kompres 168–174 KB (auto)</h3>
            <p class="text-muted">Upload beberapa ZIP (berisi folder/gambar/PDF) dan/atau file lepas (gambar/PDF).</p>
            {{if .Message}}
            <div class="alert alert-info">{{.Message}}</div>
            {{end}}
            {{if .Summary}}
            <h5>📊 Ringkasan</h5>
            <pre>{{.Summary}}</pre>
            <a class="btn btn-success" href="/download/{{.Token}}">⬇️ Download Master ZIP</a>
            {{end}}
          </div>
        </div>
      </div>
    </div>
  </div>
</body>
</html>`))

func indexHandler(w http.ResponseWriter, r *http.Request) {
	tplIndex.Execute(w, nil)
}

func processHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(200 << 20); err != nil { // 200MB
		http.Error(w, "Parse error: "+err.Error(), http.StatusBadRequest)
		return
	}

	// read settings
	cfg := map[string]string{}
	cfg["speed"] = r.FormValue("speed")
	if cfg["speed"] == "" {
		cfg["speed"] = "fast"
	}
	cfg["min_side"] = r.FormValue("min_side")
	if cfg["min_side"] == "" {
		cfg["min_side"] = strconv.Itoa(MIN_SIDE_PX)
	}
	cfg["scale_min"] = r.FormValue("scale_min")
	if cfg["scale_min"] == "" {
		cfg["scale_min"] = fmt.Sprintf("%f", SCALE_MIN)
	}
	cfg["upscale_max"] = r.FormValue("upscale_max")
	if cfg["upscale_max"] == "" {
		cfg["upscale_max"] = fmt.Sprintf("%f", UPSCALE_MAX)
	}
	cfg["sharpen"] = "0"
	if r.FormValue("sharpen") == "on" {
		cfg["sharpen"] = "1"
	}
	cfg["sharpen_amount"] = r.FormValue("sharpen_amount")
	if cfg["sharpen_amount"] == "" {
		cfg["sharpen_amount"] = fmt.Sprintf("%f", SHARPEN_AMOUNT)
	}
	masterName := r.FormValue("master_name")
	if masterName == "" {
		masterName = MASTER_ZIP_NAME
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		tplIndex.Execute(w, map[string]interface{}{"Message": "Silakan upload minimal satu file."})
		return
	}

	// Collect jobs
	type Job struct {
		Label string
		Rel   string
		Data  []byte
	}
	jobs := []Job{}
	usedLabels := map[string]int{}

	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(f)
		f.Close()
		name := fh.Filename

		if strings.HasSuffix(strings.ToLower(name), ".zip") && ALLOW_ZIP {
			pairs, err := extractZipToMemory(b)
			if err != nil {
				log.Printf("failed unzip %s: %v", name, err)
				continue
			}
			base := strings.TrimSuffix(name, filepath.Ext(name))
			if base == "" {
				base = "output"
			}
			idx := 1
			for i := range pairs {
				rel := pairs[i].Rel
				if _, ok := IMG_EXT[strings.ToLower(filepath.Ext(rel))]; ok || PDF_EXT[strings.ToLower(filepath.Ext(rel))] {
					lbl := base
					if usedLabels[lbl] > 0 {
						lbl = fmt.Sprintf("%s_%d", base, usedLabels[base]+1)
					}
					usedLabels[base]++
					jobs = append(jobs, Job{Label: lbl, Rel: rel, Data: pairs[i].Data})
				}
				idx++
			}
		} else {
			ext := strings.ToLower(filepath.Ext(name))
			if IMG_EXT[ext] || PDF_EXT[ext] {
				base := fmt.Sprintf("compressed_pict_%d", time.Now().Unix())
				jobs = append(jobs, Job{Label: base, Rel: name, Data: b})
			}
		}
	}

	if len(jobs) == 0 {
		tplIndex.Execute(w, map[string]interface{}{"Message": "Tidak ada berkas valid (butuh gambar/PDF, atau ZIP berisi file-file tersebut)."})
		return
	}

	// create master zip in-memory
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	summaryLines := []string{}
	skippedAll := map[string][]string{}
	sem := make(chan struct{}, THREADS)
	wg := sync.WaitGroup{}
	mu := sync.Mutex{}

	for _, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(job Job) {
			defer wg.Done()
			label := job.Label
			lblFolder := label + "_compressed"
			// write folder entry
			mu.Lock()
			zw.Create(lblFolder + "/")
			mu.Unlock()

			labelKey, processed, skipped, outs := processOneFileEntry(job.Rel, job.Data, label, cfg)
			for _, s := range processed {
				summaryLines = append(summaryLines, fmt.Sprintf("%s: %s", labelKey, s))
			}
			if len(skipped) > 0 {
				skippedAll[labelKey] = append(skippedAll[labelKey], skipped...)
			}
			// write outputs to zip
			mu.Lock()
			for rel, data := range outs {
				fpath := filepath.Join(lblFolder, rel)
				fw, _ := zw.Create(fpath)
				fw.Write(data)
			}
			mu.Unlock()
			<-sem
		}(job)
	}
	wg.Wait()
	zw.Close()

	// store zip in memory with token
	token := fmt.Sprintf("t%d", time.Now().UnixNano())
	memZips.Lock()
	memZips.m[token] = buf.Bytes()
	memZips.Unlock()

	summaryText := strings.Join(summaryLines, "\n")
	// show result page
	tplIndex.Execute(w, map[string]interface{}{"Summary": summaryText, "Token": token})
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.URL.Path, "/download/")
	memZips.RLock()
	data, ok := memZips.m[tok]
	memZips.RUnlock()
	if !ok {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=compressed.zip")
	w.Write(data)
}

func main() {
	// check env overrides
	if v := os.Getenv("SPEED_PRESET"); v != "" {
		SPEED_PRESET = v
	}
	if v := os.Getenv("THREADS"); v != "" {
		if t, err := strconv.Atoi(v); err == nil {
			THREADS = t
		}
	}

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/process", processHandler)
	http.HandleFunc("/download/", downloadHandler)

	addr := ":8080"
	log.Printf("Server listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
