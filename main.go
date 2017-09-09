package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"github.com/disintegration/imaging"
	"github.com/pkg/errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

type (
	HeifInfo struct {
		Width    int
		Height   int
		Rotation int
		Tiles    int
		Rows     int
		Cols     int
	}
)

var (
	// Build* заполняются при сборке go build -ldflags
	BuildTime    string
	BuildOSUname string
	BuildCommit  string
	buildVersion string // объединение Build* в одну строку
)

var (
	argv struct {
		help    bool
		version bool

		ffmpegPath    string
		heif2hevcPath string

		pngCompression int
		jpegQuality    int

		threads int
	}
)

func init() {
	buildVersion = fmt.Sprintf(`heif2png compiled at %s by %s after %s on %s`, BuildTime, runtime.Version(),
		BuildCommit, BuildOSUname,
	)

	flag.BoolVar(&argv.help, `h`, false, `show this help`)
	flag.BoolVar(&argv.version, `version`, false, `show version`)

	flag.StringVar(&argv.ffmpegPath, `ffmpeg`, `ffmpeg`, `path to ffmpeg binary`)
	flag.StringVar(&argv.heif2hevcPath, `heif2hevc`, `heif2hevc`, `path to heif2hevc binary`)

	flag.IntVar(&argv.pngCompression, `png-compr`, 0, `png compression (0 - default, 1 - no, 2 - best speed, 3 - best compression)`)
	flag.IntVar(&argv.jpegQuality, `jpeg-qual`, 90, `jpeg quality (0 - worst, 100 - best)`)

	flag.IntVar(&argv.threads, `threads`, 1, `thread pool size`)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s: [options] /path/to/src.heif /path/to/dst.(png|jpg)\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()
}

func main() {
	if argv.version {
		fmt.Fprint(os.Stderr, buildVersion, "\n")
		return
	} else if argv.help {
		flag.Usage()
		return
	} else if (argv.pngCompression < 0 || argv.pngCompression > 3) || (argv.jpegQuality < 0 || argv.jpegQuality > 100) {
		flag.Usage()
		return
	} else if len(flag.Args()) < 2 {
		flag.Usage()
		return
	}

	argv.pngCompression = -argv.pngCompression // для формата image/png.CompressionLevel

	srcFile := flag.Arg(0)
	dstFile := flag.Arg(1)

	info, err := heifGetInfo(srcFile)
	if err != nil {
		panic(err)
	}
	if info.Tiles == 1 {
		info.Cols = 1
		info.Rows = 1
	}

	// достаем исходные тайлы
	hevcFiles, err := heif2hevc(srcFile, dstFile)
	if hevcFiles != nil {
		defer func() {
			for _, f := range hevcFiles {
				os.Remove(f)
			}
		}()
	}
	if err != nil {
		panic(err)
	}

	type QueueItem struct {
		hevcFile string
		x, y     int
	}

	queue := make(chan QueueItem, len(hevcFiles))
	for i, f := range hevcFiles {
		queue <- QueueItem{
			hevcFile: f,
			x:        i % info.Cols,
			y:        i / info.Cols,
		}
	}
	close(queue)

	var (
		wg           sync.WaitGroup
		processError error
		dstImg       draw.Image = image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))
	)

	// hevc -> Image
	for t := 0; t < argv.threads; t++ {
		wg.Add(1)
		go func() {
			var point image.Point
			for f := range queue {
				if img, err := hevc2Image(f.hevcFile); err != nil {
					processError = err
				} else {
					rect := img.Bounds()
					rect.Min.X = f.x * rect.Max.X
					rect.Min.Y = f.y * rect.Max.Y
					rect.Max.X += rect.Min.X
					rect.Max.Y += rect.Min.Y
					draw.Draw(dstImg, rect, img, point, draw.Src)
				}
			}
			wg.Done()
		}()
	}
	wg.Wait()

	// поворот
	if info.Rotation != 0 {
		dstImg = imaging.Rotate(dstImg, float64(info.Rotation), color.Alpha{})
	}

	// итоговая картинка
	if fd, err := os.Create(dstFile); err != nil {
		fmt.Fprintln(os.Stderr, `Cannot create dst file`, err)
	} else {
		ext := strings.ToLower(filepath.Ext(dstFile))
		switch ext {
		case `.png`:
			encoder := png.Encoder{}
			encoder.CompressionLevel = png.CompressionLevel(argv.pngCompression)
			if err := encoder.Encode(fd, dstImg); err != nil {
				fmt.Fprintln(os.Stderr, `Cannot write dst file`, err)
			}
		case `.jpg`, `.jpeg`:
			var opt jpeg.Options
			opt.Quality = argv.jpegQuality
			if err := jpeg.Encode(fd, dstImg, &opt); err != nil {
				fmt.Fprintln(os.Stderr, `Cannot write dst file`, err)
			}
		default:
			fmt.Fprintln(os.Stderr, `Unsupported dst file extension`, err)
		}

		fd.Close()
	}
}

func heifGetInfo(srcFile string) (info HeifInfo, err error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	cmd := exec.Command(argv.heif2hevcPath, `-info`, srcFile)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, `heif2hevc -info fail:`, string(stderr.Bytes()))
		return info, errors.Wrap(err, `exec heif2hevc -info fail`)
	}

	splitInt := func(line []byte) (name []byte, val int) {
		if pos := bytes.IndexByte(line, '='); pos == -1 {
			return nil, 0
		} else if val, err := strconv.ParseInt(string(bytes.TrimSpace(line[pos+1:])), 10, 32); err != nil {
			return nil, 0
		} else {
			return line[:pos], int(val)
		}
	}

	rd := bufio.NewReader(&stdout)
	for {
		if line, err := rd.ReadBytes('\n'); err != nil {
			break // io.EOF
		} else if name, val := splitInt(line); name == nil {
			continue
		} else if bytes.Equal(name, []byte(`width`)) {
			info.Width = val
		} else if bytes.Equal(name, []byte(`height`)) {
			info.Height = val
		} else if bytes.Equal(name, []byte(`rotation`)) {
			info.Rotation = val
		} else if bytes.Equal(name, []byte(`tiles`)) {
			info.Tiles = val
		} else if bytes.Equal(name, []byte(`rows`)) {
			info.Rows = val
		} else if bytes.Equal(name, []byte(`cols`)) {
			info.Cols = val
		}
	}

	return
}

func heif2hevc(srcFile, dstFile string) (dstFiles []string, err error) {
	dstFileTmp := fmt.Sprintf(`%s.%d.tmp`, dstFile, os.Getpid())

	defer func() {
		dstFiles, _ = filepath.Glob(dstFileTmp + `*`)
	}()

	if out, err := exec.Command(argv.heif2hevcPath, srcFile, dstFileTmp).CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, `heif2hevc fail:`, string(out))
		return nil, errors.Wrap(err, `exec heif2hevc fail`)
	}

	return
}

func hevc2Image(srcFile string) (img image.Image, err error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(argv.ffmpegPath, `-hide_banner`, `-f`, `hevc`, `-i`, srcFile, `-f`, `image2pipe`, `-vcodec`, `png`, `-`)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, `ffmpeg fail:`, string(stderr.Bytes()))
		return nil, errors.Wrap(err, `ffmpeg fail`)
	}

	bb := stdout.Bytes()

	if img, err = png.Decode(&stdout); err != nil {
		fmt.Fprintln(os.Stderr, `ffmpeg fail:`, string(bb))
		err = errors.Wrap(err, `png decode fail`)
	}

	return
}
