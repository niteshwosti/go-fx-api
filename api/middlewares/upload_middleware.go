package middlewares

import (
	"bytes"
	"clean-architecture/api/responses"
	"clean-architecture/constants"
	"clean-architecture/lib"
	"clean-architecture/services"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/chai2010/webp"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nfnt/resize"
	"golang.org/x/sync/errgroup"
)

type Extension string

const (
	JPEGFile Extension = ".jpeg"
	JPGFile  Extension = ".jpg"
	PNGFile  Extension = ".png"
)

var (
	ErrExtensionMismatch      = errors.New("file extension not supported")
	ErrThumbExtensionMismatch = errors.New("file extension not supported for thumbnail")
	ErrFileRead               = errors.New("file read error")
)

type UploadConfig struct {
	// FieldName where to pull multipart files from
	FieldName string

	// BucketFolder where to put the uploaded files to
	BucketFolder string

	// Extensions array of extensions
	Extensions []Extension

	// ThumbnailEnabled set whether thumbnail is enabled or nor
	ThumbnailEnabled bool

	// ThumbnailWidth set thumbnail width
	ThumbnailWidth uint

	// WebpEnabled set whether thumbnail is enabled or nor
	WebpEnabled bool
}

type UploadMiddleware struct {
	logger lib.Logger
	bucket services.BucketService
	config []UploadConfig
}

func NewUploadMiddleware(
	logger lib.Logger,
	bucket services.BucketService,
) UploadMiddleware {
	m := UploadMiddleware{
		bucket: bucket,
		logger: logger,
	}
	return m
}

func (u UploadMiddleware) Config() UploadConfig {
	return UploadConfig{
		FieldName:        "file",
		BucketFolder:     "",
		Extensions:       []Extension{JPEGFile, PNGFile, JPGFile},
		ThumbnailEnabled: false,
		ThumbnailWidth:   100,
	}
}

// Field modify field of upload
func (cfg UploadConfig) Field(name string) UploadConfig {
	cfg.FieldName = name
	return cfg
}

// Folder modify folder of upload
func (cfg UploadConfig) Folder(folder string) UploadConfig {
	cfg.BucketFolder = folder
	return cfg
}

// Extension modify upload extension
func (cfg UploadConfig) Extension(ext ...Extension) UploadConfig {
	cfg.Extensions = ext
	return cfg
}

// ThumbEnable enable thumbnail generation
func (cfg UploadConfig) ThumbEnable(enable bool) UploadConfig {
	cfg.ThumbnailEnabled = enable
	return cfg
}

// WEBpEnabled enable thumbnail generation
func (cfg UploadConfig) WebpEnable(enable bool) UploadConfig {
	cfg.WebpEnabled = enable
	return cfg
}

// Push adds file upload configuration
func (u UploadMiddleware) Push(config UploadConfig) UploadMiddleware {
	u.config = append(u.config, config)
	return u
}

// Handle handles file upload
func (u UploadMiddleware) Handle() gin.HandlerFunc {
	return func(c *gin.Context) {

		if len(u.config) == 0 {
			u.logger.Info("no file upload configuration has been attached")
		}

		errGroup, ctx := errgroup.WithContext(c.Request.Context())

		uploadedFiles := []lib.UploadMetadata{}

		for i := range u.config {
			conf := u.config[i]

			file, fileHeader, _ := c.Request.FormFile(conf.FieldName)

			if file != nil && fileHeader != nil {

				ext := filepath.Ext(fileHeader.Filename)
				if !u.matchesExtension(conf, ext) {
					u.logger.Error("file-upload-error: ", ErrExtensionMismatch.Error())
					responses.ErrorJSON(c, http.StatusInternalServerError, ErrExtensionMismatch.Error())
					c.Abort()
					return
				}

				fileByte, err := ioutil.ReadAll(file)
				if err != nil {
					u.logger.Error("file-upload-error: ", ErrFileRead)
					responses.ErrorJSON(c, http.StatusInternalServerError, ErrFileRead)
					c.Abort()
					return
				}

				uploadFileName, fileUID := u.randomFileName(conf, ext)
				fileReader := bytes.NewReader(fileByte)
				errGroup.Go(func() error {
					urlResponse, err := u.bucket.UploadFile(ctx, fileReader, uploadFileName, fileHeader.Filename)
					uploadedFiles = append(uploadedFiles, lib.UploadMetadata{
						FieldName: conf.FieldName,
						FileName:  fileHeader.Filename,
						URL:       urlResponse,
						FileUID:   fileUID,
						Size:      fileHeader.Size,
					})
					return err
				})

				// original image
				if conf.WebpEnabled && u.properExtension(ext) {
					origWebpReader := bytes.NewReader(fileByte)
					errGroup.Go(func() error {
						var webpBuf bytes.Buffer
						img, err := u.getImage(origWebpReader, ext)
						if err != nil {
							return err
						}

						if err := webp.Encode(&webpBuf, img, &webp.Options{Lossless: true}); err != nil {
							return err
						}

						webpReader := bytes.NewReader(webpBuf.Bytes())
						resizeFileName := u.bucketPath(conf, fmt.Sprintf("%s_webp%s", fileUID, ext))

						if _, err := u.bucket.UploadFile(ctx, webpReader, resizeFileName, strings.ReplaceAll(fileHeader.Filename, ext, "")+".webp"); err != nil {
							return err
						}

						return nil
					})
				}

				if conf.ThumbnailEnabled {
					thumbReader := bytes.NewReader(fileByte)
					errGroup.Go(func() error {
						if !u.properExtension(ext) {
							return ErrExtensionMismatch
						}
						// Genrate non-webp thumbnail
						img, err := u.createThumbnail(conf, thumbReader, ext)
						if err != nil {
							return err
						}

						resizeFileName := u.bucketPath(conf, fmt.Sprintf("%s_thumb%s", fileUID, ext))
						_, err = u.bucket.UploadFile(ctx, img, resizeFileName, fileHeader.Filename)
						if err != nil {
							return err
						}
						return nil
					})

					if conf.WebpEnabled && u.properExtension(ext) {
						webpReader := bytes.NewReader(fileByte)
						errGroup.Go(func() error {
							var webpBuf bytes.Buffer
							img, err := u.getImage(webpReader, ext)
							if err != nil {
								return err
							}

							resizeImage := resize.Resize(conf.ThumbnailWidth, 0, img, resize.Lanczos3)
							if err := webp.Encode(&webpBuf, resizeImage, &webp.Options{Lossless: true}); err != nil {
								return err
							}

							webpReader := bytes.NewReader(webpBuf.Bytes())
							resizeFileName := u.bucketPath(conf, fmt.Sprintf("%s_thumb%s", fileUID, ".webp"))

							_, err = u.bucket.UploadFile(ctx, webpReader, resizeFileName, fileHeader.Filename)
							if err != nil {
								return err
							}

							return nil
						})
					}
				}
			}
		}
		if err := errGroup.Wait(); err != nil {
			u.logger.Error("file-upload-error: ", err.Error())
			if err == ErrThumbExtensionMismatch {
				responses.ErrorJSON(c, http.StatusBadRequest, err)
			} else {
				responses.ErrorJSON(c, http.StatusInternalServerError, err)
			}
			c.Abort()
			return
		}

		c.Set(constants.File, lib.UploadedFiles(uploadedFiles))
		c.Next()

	}
}

func (u UploadMiddleware) properExtension(ext string) bool {
	e := Extension(ext)
	return e == JPEGFile || e == JPGFile || e == PNGFile
}

func (u UploadMiddleware) matchesExtension(c UploadConfig, ext string) bool {
	for _, e := range c.Extensions {
		if e == Extension(ext) {
			return true
		}
	}
	return false
}

func (u UploadMiddleware) randomFileName(c UploadConfig, ext string) (string, string) {
	randUUID, _ := uuid.NewRandom()
	fileName := randUUID.String() + ext
	return u.bucketPath(c, fileName), randUUID.String()
}

func (u UploadMiddleware) bucketPath(c UploadConfig, fileName string) string {
	if c.BucketFolder != "" {
		return fmt.Sprintf("%s/%s", c.BucketFolder, fileName)
	}
	return fileName
}

func (u UploadMiddleware) getImage(file io.Reader, ext string) (image.Image, error) {
	if Extension(ext) == JPGFile || Extension(ext) == JPEGFile {
		return jpeg.Decode(file)
	}
	if Extension(ext) == PNGFile {
		return png.Decode(file)
	}
	return nil, ErrExtensionMismatch
}

// createThumbnail creates thumbnail from multipart file
func (u UploadMiddleware) createThumbnail(c UploadConfig, file io.Reader, ext string) (*bytes.Buffer, error) {
	img, err := u.getImage(file, ext)
	if err != nil {
		return nil, err
	}

	resizeImage := resize.Resize(c.ThumbnailWidth, 0, img, resize.Lanczos3)
	buff := new(bytes.Buffer)
	if Extension(ext) == JPGFile || Extension(ext) == JPEGFile {
		if err := jpeg.Encode(buff, resizeImage, nil); err != nil {
			return nil, err
		}
	}
	if Extension(ext) == PNGFile {
		if err := png.Encode(buff, resizeImage); err != nil {
			return nil, err
		}
	}

	return buff, nil
}
