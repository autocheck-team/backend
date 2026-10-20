// Package storage — объектное хранилище (S3-совместимое, SeaweedFS) для исходников,
// тестов и артефактов проверки.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrNotFound — объекта нет.
var ErrNotFound = errors.New("object not found")

// region фиксирован, чтобы клиент не ходил в хранилище за регионом бакета:
// presigned-ссылки подписываются без сетевых запросов.
const region = "us-east-1"

// Config — подключение к хранилищу.
type Config struct {
	// Endpoint — адрес для бэкенда, например "seaweedfs:8333" внутри compose.
	Endpoint string
	// PublicEndpoint — адрес, по которому хранилище доступно браузеру; на него
	// выписываются presigned-ссылки. Пустой — тот же, что Endpoint.
	PublicEndpoint string
	Bucket         string
	AccessKey      string
	SecretKey      string
	UseSSL         bool
}

// Storage — бакет в S3.
type Storage struct {
	client *minio.Client
	public *minio.Client
	bucket string
}

// New подключается к хранилищу и создаёт бакет, если его нет.
func New(ctx context.Context, cfg Config) (*Storage, error) {
	opts := func() *minio.Options {
		return &minio.Options{
			Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
			Secure:       cfg.UseSSL,
			Region:       region,
			BucketLookup: minio.BucketLookupPath,
		}
	}
	client, err := minio.New(cfg.Endpoint, opts())
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}
	public := client
	if cfg.PublicEndpoint != "" && cfg.PublicEndpoint != cfg.Endpoint {
		if public, err = minio.New(cfg.PublicEndpoint, opts()); err != nil {
			return nil, fmt.Errorf("create public s3 client: %w", err)
		}
	}

	s := &Storage{client: client, public: public, bucket: cfg.Bucket}
	if err := s.ensureBucket(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Storage) ensureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("check bucket %s: %w", s.bucket, err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: region}); err != nil {
		return fmt.Errorf("create bucket %s: %w", s.bucket, err)
	}
	return nil
}

// PresignPut возвращает ссылку для загрузки объекта через HTTP PUT.
func (s *Storage) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.public.PresignedPutObject(ctx, s.bucket, key, ttl)
	if err != nil {
		return "", fmt.Errorf("presign put %s: %w", key, err)
	}
	return u.String(), nil
}

// PresignGet возвращает ссылку для скачивания объекта.
func (s *Storage) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.public.PresignedGetObject(ctx, s.bucket, key, ttl, nil)
	if err != nil {
		return "", fmt.Errorf("presign get %s: %w", key, err)
	}
	return u.String(), nil
}

// Size возвращает размер объекта или ErrNotFound.
func (s *Storage) Size(ctx context.Context, key string) (int64, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("stat %s: %w", key, err)
	}
	return info.Size, nil
}

// Put сохраняет объект.
func (s *Storage) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// Get открывает объект на чтение. Вызывающий закрывает reader.
func (s *Storage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	// GetObject ленивый: ошибка «нет объекта» видна только при первом обращении.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	return obj, nil
}

// DeletePrefix удаляет все объекты с префиксом.
func (s *Storage) DeletePrefix(ctx context.Context, prefix string) error {
	objects := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
	for e := range s.client.RemoveObjects(ctx, s.bucket, objects, minio.RemoveObjectsOptions{}) {
		if e.Err != nil {
			return fmt.Errorf("delete %s: %w", e.ObjectName, e.Err)
		}
	}
	return nil
}
