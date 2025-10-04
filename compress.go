package main

import (
    "archive/zip"
    "bytes"
    "fmt"
    "image"
    "image/jpeg"
    "io"
    "log"
    "mime/multipart"
    "net/http"
    "path/filepath"
    "strconv"
    "strings"
    "sync"

    "github.com/disintegration/imaging"
    "github.com/gin-gonic/gin"
    "github.com/gen2brain/go-fitz"
)

const (
    MinKB     = 168
    TargetKB  = 174
    MaxQuality = 95
    MinQuality = 15
    MinSidePx  = 256
)

func main() {
    r := gin.Default()

    // Serve HTML upload form
    r.GET("/", func(c *gin.Context) {
        c.Header("Content-Type", "text/html")
        c.String(http.StatusOK, uploadPage)
    })

    r.POST("/upload", func(c *gin.Context) {
        form, err := c.MultipartForm()
        if err != nil {
            c.String(http.StatusBadRequest, "Failed to parse form: %v", err)
            return
        }

        files := form.File["files"]
        if len(files) == 0 {
            c.String(http.StatusBadRequest, "No files uploaded")
            return
        }

        var wg sync.WaitGroup
        var mu sync.Mutex
        masterZipBuf := new(bytes.Buffer)
        zw := zip.NewWriter(masterZipBuf)

        var processingSummary []string

        for _, fileHeader := range files {
            wg.Add(1)
            go func(fh *multipart.FileHeader) {
                defer wg.Done()
                file, err := fh.Open()
                if err != nil {
                    mu.Lock()
                    processingSummary = append(processingSummary, fmt.Sprintf("Error opening %s: %v", fh.Filename, err))
                    mu.Unlock()
                    return
                }
                defer file.Close()

                ext := strings.ToLower(filepath.Ext(fh.Filename))
                var fileBuf bytes.Buffer
                if _, err := io.Copy(&fileBuf, file); err != nil {
                    mu.Lock()
                    processingSummary = append(processingSummary, fmt.Sprintf("Error reading %s: %v", fh.Filename, err))
                    mu.Unlock()
                    return
                }

                if ext == ".zip" {
                    // Extract ZIP and process contained images/PDFs
                    r := bytes.NewReader(fileBuf.Bytes())
                    zf, err := zip.NewReader(r, int64(r.Len()))
                    if err != nil {
                        mu.Lock()
                        processingSummary = append(processingSummary, fmt.Sprintf("Error reading ZIP %s: %v", fh.Filename, err))
                        mu.Unlock()
                        return
                    }
                    for _, f := range zf.File {
                        if f.FileInfo().IsDir() {
                            continue
                        }
                        extInner := strings.ToLower(filepath.Ext(f.Name))
                        if isSupportedFile(extInner) {
                            rc, err := f.Open()
                            if err != nil {
                                mu.Lock()
                                processingSummary = append(processingSummary, fmt.Sprintf("Error reading inner file %s: %v", f.Name, err))
                                mu.Unlock()
                                continue
                            }
                            var innerBuf bytes.Buffer
                            io.Copy(&innerBuf, rc)
                            rc.Close()
                            processAndAddToZip(innerBuf.Bytes(), f.Name, zw, &mu, &processingSummary)
                        }
                    }
                } else if isSupportedFile(ext) {
                    processAndAddToZip(fileBuf.Bytes(), fh.Filename, zw, &mu, &processingSummary)
                } else {
                    mu.Lock()
                    processingSummary = append(processingSummary, fmt.Sprintf("Skipped unsupported file: %s", fh.Filename))
                    mu.Unlock()
                }
            }(fileHeader)
        }

        wg.Wait()
        zw.Close()

        c.Header("Content-Type", "application/zip")
        c.Header("Content-Disposition", "attachment; filename=compressed.zip")
        c.Writer.Write(masterZipBuf.Bytes())
    })

    r.Run(":8080")
}

func isSupportedFile(ext string) bool {
    switch ext {
    case ".jpg", ".jpeg", ".png", ".gif", ".pdf":
        return true
    default:
        return false
    }
}

func processAndAddToZip(fileBytes []byte, filename string, zw *zip.Writer, mu *sync.Mutex, summary *[]string) {
    ext := strings.ToLower(filepath.Ext(filename))
    baseName := strings.TrimSuffix(filename, ext)

    if ext == ".pdf" {
        // Extract images from PDF pages
        doc, err := fitz.NewFromMemory(fileBytes)
        if err != nil {
            mu.Lock()
            *summary = append(*summary, fmt.Sprintf("Failed to open PDF %s: %v", filename, err))
            mu.Unlock()
            return
        }
        defer doc.Close()
        for i := 0; i < doc.NumPage(); i++ {
            img, err := doc.Image(i)
            if err != nil {
                mu.Lock()
                *summary = append(*summary, fmt.Sprintf("Failed to render PDF page %d: %v", i, err))
                mu.Unlock()
                continue
            }
            processedImgBytes := compressImage(img)
            entryName := fmt.Sprintf("%s_p%d.jpg", baseName, i+1)
            mu.Lock()
            w, err := zw.Create(entryName)
            if err == nil {
                w.Write(processedImgBytes)
                *summary = append(*summary, fmt.Sprintf("Processed %s (PDF page %d)", filename, i+1))
            } else {
                *summary = append(*summary, fmt.Sprintf("Failed to add to zip: %v", err))
            }
            mu.Unlock()
        }
    } else {
        img, _, err := image.Decode(bytes.NewReader(fileBytes))
        if err != nil {
            mu.Lock()
            *summary = append(*summary, fmt.Sprintf("Failed to decode image %s: %v", filename, err))
            mu.Unlock()
            return
        }
        processedImgBytes := compressImage(img)
        outName := baseName + ".jpg"
        mu.Lock()
        w, err := zw.Create(outName)
        if err == nil {
            w.Write(processedImgBytes)
            *summary = append(*summary, fmt.Sprintf("Processed %s", filename))
        } else {
            *summary = append(*summary, fmt.Sprintf("Failed to add to zip: %v", err))
        }
        mu.Unlock()
    }
}

func compressImage(img image.Image) []byte {
    // Resize to minimum side if needed
    w := img.Bounds().Dx()
    h := img.Bounds().Dy()
    minSide := MinSidePx
    if w < minSide || h < minSide {
        scale := float64(minSide) / float64(min(w, h))
        img = imaging.Resize(img, int(float64(w)*scale), int(float64(h)*scale), imaging.Lanczos)
    }

    // Compress trying to get approx 170KB size by reducing quality
    var buf bytes.Buffer
    quality := MaxQuality
    for quality >= MinQuality {
        buf.Reset()
        opts := &jpeg.Options{Quality: quality}
        jpeg.Encode(&buf, img, opts)
        sizeKB := buf.Len() / 1024
        if sizeKB <= TargetKB {
            break
        }
        quality -= 5
    }

    return buf.Bytes()
}

func min(a, b int) int {
    if a < b {
        return a
    }
    return b
}

const uploadPage = `
<!DOCTYPE html>
<html>
<head><title>Multi-ZIP JPG Compressor</title></head>
<body>
<h2>Upload ZIP or images/PDF files</h2>
<form enctype="multipart/form-data" action="/upload" method="post">
    <input type="file" name="files" multiple>
    <br><br>
    <button type="submit">Upload & Process</button>
</form>
</body>
</html>
`
