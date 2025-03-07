package main

import (
	"bytes"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	"golang.org/x/sync/errgroup"
)

// quick function to make some boilerplate easier
func ginfn(fn func(*gin.Context) error) func(*gin.Context) {
	return func(c *gin.Context) {
		err := fn(c)
		if err != nil {
			c.Error(err)
		}
	}
}

func run() (err error) {
	tmpdir, err := os.MkdirTemp(os.TempDir(), "dicomserving")
	if err != nil {
		return
	}
	storage, err := os.OpenRoot(tmpdir)
	if err != nil {
		return err
	}
	defer storage.Close()

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery(), gin.ErrorLogger())
	// preserve ip address under istio/trusted proxies
	r.SetTrustedProxies([]string{"127.0.0.0/8", "::1"})

	r.GET("/:id", func(ctx *gin.Context) {
		ctx.FileFromFS(ctx.Param("id"), http.FS(storage.FS()))
	})

	r.PUT("/:id", ginfn(func(ctx *gin.Context) (err error) {
		file, err := storage.Create(ctx.Param("id"))
		if err != nil {
			return
		}
		defer file.Close()

		_, err = io.Copy(file, ctx.Request.Body)
		return
	}))

	r.GET("/:id/tag", ginfn(func(ctx *gin.Context) (err error) {
		tag, err := tag.FindByName(ctx.Query("name"))
		if err != nil {
			ctx.Status(http.StatusBadRequest)
			return
		}

		file, err := storage.Open(ctx.Param("id"))
		if err != nil {
			return
		}
		defer file.Close()

		dcom, err := dicom.ParseUntilEOF(file, nil)
		if err != nil {
			return
		}

		elem, err := dcom.FindElementByTagNested(tag.Tag)
		if err != nil {
			return
		}

		ctx.JSON(http.StatusOK, elem)
		return
	}))

	r.GET("/:id/image", ginfn(func(ctx *gin.Context) (err error) {
		file, err := storage.Open(ctx.Param("id"))
		if err != nil {
			return
		}
		defer file.Close()

		framechan := make(chan *frame.Frame)
		grp, c := errgroup.WithContext(ctx)

		grp.Go(func() (err error) {
			_, err = dicom.ParseUntilEOF(file, framechan)
			return
		})

		grp.Go(func() (err error) {
			f, ok := <-framechan
			if !ok {
				ctx.String(http.StatusNoContent, "no image content found")
				return
			}

			// drain the framechan so it doesn't get
			// backed up and halt the parser
			grp.Go(func() error {
				for {
					select {
					case <-c.Done():
						return c.Err()
					case _, ok := <-framechan:
						if !ok {
							return nil
						}
					}
				}
			})

			img, err := f.GetImage()
			if err != nil {
				return
			}

			buf := bytes.NewBuffer(nil)
			err = png.Encode(buf, img)
			if err != nil {
				return
			}

			ctx.DataFromReader(http.StatusOK, int64(buf.Len()), http.DetectContentType(buf.Bytes()), buf, nil)
			return
		})

		return grp.Wait()
	}))
	return r.Run(":8080")
}

func main() {
	log.Fatal(run())
}
