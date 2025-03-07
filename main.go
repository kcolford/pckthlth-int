package main

import (
	"bytes"
	"errors"
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

type StatusError struct {
	internal error
	status   int
}

func NewStatusError(status int, wrapped error) StatusError {
	return StatusError{
		internal: wrapped,
		status:   status,
	}
}

func (s StatusError) Error() string {
	return s.internal.Error()
}

func (s StatusError) Unwrap() error {
	return s.internal
}

func (s StatusError) Is(target error) bool {
	_, ok := target.(StatusError)
	return ok
}

/* quick function to make some boilerplate easier */
func ginfn(fn func(*gin.Context) error) func(*gin.Context) {
	return func(c *gin.Context) {
		err := fn(c)
		if err != nil {
			c.Error(err)
		}
	}
}

func ErrorHandler(c *gin.Context) {
	c.Next()
	err := c.Errors.Last()
	if err != nil {
		status := http.StatusInternalServerError
		var s StatusError
		if errors.As(err, &s) {
			status = s.status
		}
		c.JSON(status, c.Errors)
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

	r := gin.Default()

	/* preserve ip address under istio/trusted proxies */
	r.SetTrustedProxies([]string{"127.0.0.0/8", "::1"})

	r.Use(ErrorHandler)

	r.GET("/:id", ginfn(func(ctx *gin.Context) (err error) {
		ctx.FileFromFS(ctx.Param("id"), http.FS(storage.FS()))
		return
	}))

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
			return NewStatusError(http.StatusBadRequest, err)
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
				return NewStatusError(http.StatusNoContent, errors.New("no image content found"))
			}

			/* drain the framechan so it doesn't get backed up and halt the parser */
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
