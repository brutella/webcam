// This linux program provides access to v4l2 video devices
// via the http enpdoints `/image` and `/video`.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/blackjack/webcam"
	"golang.org/x/image/draw"
)

const (
	V4L2_PIX_FMT_PJPG = 0x47504A50
	V4L2_PIX_FMT_MJPG = 0x47504A4D
	V4L2_PIX_FMT_YUYV = 0x56595559
)

type FrameSizes []webcam.FrameSize

func (slice FrameSizes) Len() int {
	return len(slice)
}

// For sorting purposes
func (slice FrameSizes) Less(i, j int) bool {
	ls := slice[i].MaxWidth * slice[i].MaxHeight
	rs := slice[j].MaxWidth * slice[j].MaxHeight
	return ls < rs
}

// For sorting purposes
func (slice FrameSizes) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

var supportedFormats = map[webcam.PixelFormat]bool{
	V4L2_PIX_FMT_PJPG: true,
	V4L2_PIX_FMT_YUYV: true,
	V4L2_PIX_FMT_MJPG: true,
}

func main() {
	dev := flag.String("d", "/dev/video0", "video device to use")
	fmtstr := flag.String("f", "", "video format to use, default first supported")
	szstr := flag.String("s", "", "frame size to use, default largest one")
	addr := flag.String("l", ":8080", "addr to listen")
	fps := flag.Bool("p", false, "print fps info")
	flag.Parse()

	// modprobe the uvcvideo driver
	for _, mod := range []string{
		"kernel/drivers/media/common/videobuf2/videobuf2-common.ko",
		"kernel/drivers/media/common/videobuf2/videobuf2-v4l2.ko",
		"kernel/drivers/media/common/uvc.ko",
		"kernel/drivers/media/common/videobuf2/videobuf2-memops.ko",
		"kernel/drivers/media/common/videobuf2/videobuf2-vmalloc.ko",
		"kernel/drivers/media/usb/uvc/uvcvideo.ko",
	} {
		if err := loadModule(mod); err != nil && !os.IsNotExist(err) {
			log.Fatal(err)
		}
	}

	log.Println("kernel modules loaded")

	cam, err := webcam.Open(*dev)
	if err != nil {
		log.Fatal(err)
	}
	defer cam.Close()

	// select pixel format
	format_desc := cam.GetSupportedFormats()

	fmt.Println("Available formats:")
	for _, s := range format_desc {
		fmt.Fprintln(os.Stderr, s)
	}

	var format webcam.PixelFormat
FMT:
	for f, s := range format_desc {
		if *fmtstr == "" {
			if supportedFormats[f] {
				format = f
				break FMT
			}

		} else if *fmtstr == s {
			if !supportedFormats[f] {
				log.Fatalln(format_desc[f], "format is not supported, exiting")
			}
			format = f
			break
		}
	}
	if format == 0 {
		log.Fatal("No format found, exiting")
	}

	// select frame size
	frames := FrameSizes(cam.GetSupportedFrameSizes(format))
	sort.Sort(frames)

	fmt.Fprintln(os.Stderr, "Supported frame sizes for format", format_desc[format])
	for _, f := range frames {
		fmt.Fprintln(os.Stderr, f.GetString())
	}
	var size *webcam.FrameSize
	if *szstr == "" {
		size = &frames[len(frames)-1]
	} else {
		for _, f := range frames {
			if *szstr == f.GetString() {
				size = &f
				break
			}
		}
	}
	if size == nil {
		log.Fatal("No matching frame size, exiting")
	}

	fmt.Fprintln(os.Stderr, "Requesting", format_desc[format], size.GetString())
	f, w, h, err := cam.SetImageFormat(format, uint32(size.MaxWidth), uint32(size.MaxHeight))
	if err != nil {
		log.Fatal("SetImageFormat return error", err)

	}
	fmt.Fprintf(os.Stderr, "Resulting image format: %s %dx%d\n", format_desc[f], w, h)

	fmt.Println("Supported framerates for", format, size)
	for _, rate := range cam.GetSupportedFramerates(format, uint32(size.MaxWidth), uint32(size.MaxHeight)) {
		fmt.Println(rate)
	}

	// start streaming
	err = cam.StartStreaming()
	if err != nil {
		log.Fatal(err)
	}

	var (
		li   chan *bytes.Buffer = make(chan *bytes.Buffer)
		fi   chan []byte        = make(chan []byte)
		back chan struct{}      = make(chan struct{})
	)
	go encodeToImage(cam, back, fi, li, w, h, f)
	go serveHTTP(*addr, li)

	timeout := uint32(5) // 5 seconds
	start := time.Now()
	var fr time.Duration

	for {
		err = cam.WaitForFrame(timeout)
		switch err.(type) {
		case nil:
		case *webcam.Timeout:
			log.Println(err)
			continue
		default:
			log.Fatal(err)
		}

		frame, err := cam.ReadFrame()
		if err != nil {
			log.Println(err)
			continue
		}
		if len(frame) != 0 {

			// print framerate info every 10 seconds
			fr++
			if *fps {
				if d := time.Since(start); d > time.Second*10 {
					fmt.Println(float64(fr)/(float64(d)/float64(time.Second)), "fps")
					start = time.Now()
					fr = 0
				}
			}

			select {
			case fi <- frame:
				<-back
			default:
			}
		}
	}
}

func encodeToImage(wc *webcam.Webcam, back chan struct{}, fi chan []byte, li chan *bytes.Buffer, w, h uint32, format webcam.PixelFormat) {

	var (
		frame []byte
	)
	for {
		bframe := <-fi
		// copy frame
		if len(frame) < len(bframe) {
			frame = make([]byte, len(bframe))
		}
		copy(frame, bframe)
		back <- struct{}{}

		// buf holds frame as jpeg
		buf := &bytes.Buffer{}

		switch format {
		case V4L2_PIX_FMT_YUYV:
			yuyv := image.NewYCbCr(image.Rect(0, 0, int(w), int(h)), image.YCbCrSubsampleRatio422)
			for i := range yuyv.Cb {
				ii := i * 4
				yuyv.Y[i*2] = frame[ii]
				yuyv.Y[i*2+1] = frame[ii+2]
				yuyv.Cb[i] = frame[ii+1]
				yuyv.Cr[i] = frame[ii+3]

			}
			if err := jpeg.Encode(buf, yuyv, nil); err != nil {
				log.Fatal(err)
			}
		case V4L2_PIX_FMT_MJPG, V4L2_PIX_FMT_PJPG:
			buf.Write(frame)
		default:
			log.Fatal("invalid format ?")
		}

		const N = 50
		// broadcast image up to N ready clients
		nn := 0
	FOR:
		for ; nn < N; nn++ {
			select {
			case li <- buf:
			default:
				break FOR
			}
		}
		if nn == 0 {
			li <- buf
		}

	}
}

func serveHTTP(addr string, li chan *bytes.Buffer) {
	http.HandleFunc("/image", func(w http.ResponseWriter, r *http.Request) {
		log.Println("connect from", r.RemoteAddr, r.URL)

		//remove stale image
		<-li

		img := <-li

		buf := img.Bytes()
		if str := r.FormValue("s"); str != "" {
			var w, h int
			n, _ := fmt.Sscanf(str, "%dx%d", &w, &h)
			if n == 2 {
				// Decode the image (from PNG to image.Image):
				src, _ := jpeg.Decode(img)

				// Set the expected size that you want:
				dst := image.NewRGBA(image.Rect(0, 0, w, h))

				// Resize:
				draw.NearestNeighbor.Scale(dst, dst.Rect, src, src.Bounds(), draw.Over, nil)

				var resized bytes.Buffer
				jpeg.Encode(&resized, dst, &jpeg.Options{Quality: 90})
				buf = resized.Bytes()
			}
		}

		w.Header().Set("Content-Type", "image/jpeg")

		if _, err := w.Write(buf); err != nil {
			log.Println(err)
			return
		}

	})

	http.HandleFunc("/video", func(w http.ResponseWriter, r *http.Request) {
		log.Println("connect from", r.RemoteAddr, r.URL)

		//remove stale image
		<-li
		const boundary = `frame`
		w.Header().Set("Content-Type", `multipart/x-mixed-replace;boundary=`+boundary)
		multipartWriter := multipart.NewWriter(w)
		multipartWriter.SetBoundary(boundary)
		for {
			img := <-li
			image := img.Bytes()
			iw, err := multipartWriter.CreatePart(textproto.MIMEHeader{
				"Content-type":   []string{"image/jpeg"},
				"Content-length": []string{strconv.Itoa(len(image))},
			})
			if err != nil {
				log.Println(err)
				return
			}
			_, err = iw.Write(image)
			if err != nil {
				log.Println(err)
				return
			}
		}
	})

	log.Fatal(http.ListenAndServe(addr, nil))
}
