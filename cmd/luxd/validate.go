package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/marcioapm/lux/internal/blob"
)

// servePlan is what serve has settled from its configuration before it
// opens any connection: nothing in it has touched the network.
type servePlan struct {
	db    *pgxpool.Config
	blobs *blob.Store
}

// prepareServe runs every check serve makes on its configuration before
// connecting to PostgreSQL or S3. validate runs exactly this, so a release
// whose validate passes is refused by serve only for reasons outside the
// configuration (a database or bucket it cannot reach).
func prepareServe(ctx context.Context, c config) (servePlan, error) {
	dsn, err := require(c.Database.URL, "database.url", "LUX_DATABASE_URL")
	if err != nil {
		return servePlan{}, err
	}
	bucket, err := require(c.S3.Bucket, "s3.bucket", "LUX_S3_BUCKET")
	if err != nil {
		return servePlan{}, err
	}
	// pgx's parse error quotes the string with only best-effort password
	// redaction: the message names the key and nothing of its value.
	db, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return servePlan{}, errors.New("database.url (LUX_DATABASE_URL) is not a valid PostgreSQL connection string")
	}
	// blob.New reads the AWS shared configuration (profile, region) but
	// makes no request.
	blobs, err := blob.New(ctx, blob.Config{
		Endpoint:       c.S3.Endpoint,
		PublicEndpoint: c.S3.PublicEndpoint,
		Region:         c.S3.Region,
		Bucket:         bucket,
		AccessKey:      c.S3.AccessKey,
		SecretKey:      c.S3.SecretKey,
	})
	if err != nil {
		return servePlan{}, err
	}
	// http.Server binds only after the database and bucket answer: a
	// malformed address is refused here instead. Empty is net/http's ":http".
	if c.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Listen); err != nil {
			return servePlan{}, fmt.Errorf("listen (LUX_LISTEN) %q: want host:port", c.Listen)
		}
	}
	return servePlan{db: db, blobs: blobs}, nil
}

// loadServe is the configuration half of serve: load the file and the
// environment, then prepareServe. serve and validate both start here.
func loadServe(ctx context.Context, path string) (config, string, servePlan, error) {
	c, file, err := loadConfigFile(path)
	if err != nil {
		return c, file, servePlan{}, err
	}
	plan, err := prepareServe(ctx, c)
	return c, file, plan, err
}

// validate refuses what serve would refuse on configuration grounds, and
// connects to nothing. On success it prints the file it read.
func validate(ctx context.Context, path string, stdout io.Writer) error {
	_, file, _, err := loadServe(ctx, path)
	if err != nil {
		return err
	}
	if file == "" {
		file = "no file"
	}
	_, err = fmt.Fprintln(stdout, "ok:", file)
	return err
}
