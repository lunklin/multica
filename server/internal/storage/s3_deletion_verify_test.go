package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3DeletionVerification(t *testing.T) {
	for _, status := range []int{200, 403, 404, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "HEAD" {
					t.Error("verification fetched a body")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			client := s3.NewFromConfig(aws.Config{Region: "SYN-region", Credentials: credentials.NewStaticCredentialsProvider("SYN", "SYN", "")}, func(o *s3.Options) {
				o.BaseEndpoint = aws.String(server.URL)
				o.UsePathStyle = true
				o.RetryMaxAttempts = 1
			})
			s := &S3Storage{client: client, bucket: "SYN-bucket"}
			err := s.VerifyDeleted(context.Background(), "SYN-key")
			if (err == nil) != (status == 404) {
				t.Fatalf("status %d verified=%v", status, err == nil)
			}
		})
	}
}
