package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/McKael/madon"
	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	butil "github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/xrpc"
	"github.com/karalabe/go-bluesky"
	bolt "go.etcd.io/bbolt"
	"jaytaylor.com/html2text"
)

const (
	AppName    = "@mbr@tiggi.es's Very Bad Crossposter"
	AppWebsite = "https://lobisomem.gay"

	AppIdKey     = "`appId"
	AppSecretKey = "`appSecret"
)

func main() {
	ctx := context.Background()

	instanceName := requireEnv("VBC_MASTODON_INSTANCE")
	instanceName = canonicalizeInstanceName(instanceName)
	log.Printf("Mastodon: using instance name %v", instanceName)

	storeName := envOrDefault("VBC_STORE_FILE", "vbc.bolt")
	db, err := bolt.Open(storeName, 0600, nil)
	if err != nil {
		log.Fatalf("could not open store at %v: %v", storeName, err)
	}
	log.Printf("using bolt store at %v", storeName)

	mastodonAppId := envOrNil("VBC_MASTODON_APP_ID")
	mastodonAppSecret := envOrNil("VBC_MASTODON_APP_SECRET")
	mc := initMastodonClient(db, instanceName, mastodonAppId, mastodonAppSecret)

	bskyHandle := requireEnv("VBC_BSKY_HANDLE")
	bskyAppKey := requireEnv("VBC_BSKY_APP_KEY")
	bc := initBlueskyClient(ctx, bskyHandle, bskyAppKey)

	/* Query for the account on Mastodon. */
	mastodonAccountIdStr := requireEnv("VBC_MASTODON_ACCOUNT_ID")
	mastodonAccountId, err := strconv.Atoi(mastodonAccountIdStr)
	if err != nil {
		log.Fatalf("mastodon account ID is not an integer: %v", err)
	}
	log.Printf("Mastodon: querying for user with ID %v", mastodonAccountId)

	account, err := mc.GetAccount(int64(mastodonAccountId))
	if err != nil {
		log.Fatalf("could not query for user with ID %v: %v", mastodonAccountId, err)
	}
	log.Printf("Mastodon: found account with handle @%v", account.Username)

	/* Query for the user profile on Bluesky. */
	log.Printf("Bluesky: fetching profile with handle @%v", bskyHandle)
	bskyProfile, err := bc.FetchProfile(ctx, bskyHandle)
	if err != nil {
		log.Fatalf("could not fetch profile with handle @%v: %v", bskyHandle, err)
	}

	err = handleAccount(ctx, db, mc, bc, instanceName, account, bskyProfile)
	if err != nil {
		log.Fatalf("account loop failed: %v", err)
	}
}

func handleAccount(
	ctx context.Context,
	db *bolt.DB,
	mc *madon.Client,
	bc *bluesky.Client,
	instanceName string,
	acct *madon.Account,
	bskyProfile *bluesky.Profile) error {

	userPostsKey := intToBoltKV(acct.ID)
	transactWithUserPosts := func(
		fn func(instance *bolt.Bucket, userPosts *bolt.Bucket) error,
		update bool) error {

		callback := func(tx *bolt.Tx) error {
			bucket := tx.Bucket([]byte(instanceName))
			if bucket == nil {
				log.Panicf("bucket with instance name should exist at this point")
			}

			userPosts := bucket.Bucket(userPostsKey)
			return fn(bucket, userPosts)
		}

		if update {
			return db.Update(callback)
		} else {
			return db.View(callback)
		}
	}

	/* Check to see if we're bootstrapping this account. */
	err := transactWithUserPosts(func(instance *bolt.Bucket, userPosts *bolt.Bucket) error {
		if userPosts == nil {
			log.Printf("bootstrapping account @%v", acct.Username)
			userPosts, err := instance.CreateBucket(userPostsKey)
			if err != nil {
				return err
			}

			statuses, err := mc.GetAccountStatuses(
				acct.ID,
				false,
				false,
				false,
				&madon.LimitParams{All: true})
			if err != nil {
				return err
			}

			for _, status := range statuses {
				log.Printf("    ignore: post %v made in %v", status.URL, status.CreatedAt)
				err = userPosts.Put(intToBoltKV(status.ID), []byte(`{ "cid": "", "uri": "" }`))
				if err != nil {
					return err
				}
			}
		}

		return nil
	}, true)
	if err != nil {
		return err
	}

	/* Enter the loop handling user new posts. */
	for {
		statuses, err := mc.GetAccountStatuses(
			acct.ID,
			false,
			false,
			false,
			&madon.LimitParams{Limit: 1})
		if err != nil {
			return err
		}

		for _, status := range statuses {
			ignore := false
			err = transactWithUserPosts(func(_ *bolt.Bucket, userPosts *bolt.Bucket) error {
				ignore = userPosts.Get(intToBoltKV(status.ID)) != nil
				return nil
			}, false)
			if err != nil {
				return err
			}

			if ignore {
				continue
			}
			log.Printf("Mastodon: @%v has new status to repost: %v",
				acct.Username,
				status.URL)

			bskyPostId, err := repost(ctx, db, &status, bc, bskyProfile)
			if err != nil {
				log.Printf("ERROR: failed to repost %v to Bluesky: %v", status.URL, err)
				break
			}

			err = transactWithUserPosts(func(_ *bolt.Bucket, userPosts *bolt.Bucket) error {
				return userPosts.Put(intToBoltKV(status.ID), bskyPostId)
			}, true)
			if err != nil {
				return err
			}
		}

		time.Sleep(1000000000)
	}

	return nil
}

func repost(
	ctx context.Context,
	db *bolt.DB,
	status *madon.Status,
	bc *bluesky.Client,
	bskyProfile *bluesky.Profile) ([]byte, error) {

	if status.InReplyToID != nil {
		return nil, errors.New("statuses with replies are not supported")
	}
	if len(status.MediaAttachments) != 0 {
		return nil, errors.New("statuses with attachments are not supported")
	}

	/* Try to render out the HTML we get from Mastodon into plain text. */
	text := status.Content
	pretty, err := html2text.FromString(text, html2text.Options{PrettyTables: true})
	if err == nil {
		text = pretty
	}

	/* Build the post. */
	timestamp := status.CreatedAt
	post := bsky.FeedPost{
		Text:      text,
		CreatedAt: timestamp.Format(time.RFC3339),
	}

	/* Pick the collection we're gonna post to. */
	collection := "app.bsky.feed.post"

	/* Post to Bluesky. */
	input := atproto.RepoCreateRecord_Input{
		Collection: collection,
		Record:     &butil.LexiconTypeDecoder{Val: &post},
		Repo:       bskyProfile.DID,
	}
	var output *atproto.RepoCreateRecord_Output
	err = bc.CustomCall(func(client *xrpc.Client) error {
		o, err := atproto.RepoCreateRecord(ctx, client, &input)
		if err != nil {
			return err
		}
		output = o
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("Bluesky: reposted to %v", output.Uri)

	record, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	return record, nil
}

func intToBoltKV(val int64) []byte {
	return binary.AppendVarint(make([]byte, 0), val)
}

func boltKVToInt(kv []byte) (int64, error) {
	val, err := binary.Varint(kv)
	if err < 0 {
		return 0, errors.New(fmt.Sprintf(
			"kv buffer of %v bytes would overflow int64",
			-err))
	} else if err == 0 {
		return 0, errors.New("kv buffer is too small")
	}
	return val, nil
}

func canonicalizeInstanceName(name string) string {
	u, err := url.ParseRequestURI(name)
	if err != nil {
		log.Fatalf("could not parse instance name %v as a URL: %v", name, err)
	}
	if u.Opaque != "" {
		log.Fatalf("no support for opaque URL: %v", u)
	}
	u.Scheme = "https"
	u.Path = "/"
	u.RawQuery = ""
	u.RawFragment = ""
	return u.String()
}

func initBlueskyClient(ctx context.Context, handle string, appKey string) *bluesky.Client {
	log.Printf("Bluesky: connecting to %v", bluesky.ServerBskySocial)
	bc, err := bluesky.Dial(ctx, bluesky.ServerBskySocial)
	if err != nil {
		log.Fatalf("could not connect to %v: %v", bluesky.ServerBskySocial, err)
	}

	log.Printf("Bluesky: logging in as @%v", handle)
	err = bc.Login(ctx, handle, appKey)
	if err != nil {
		log.Fatalf("could not login to %v: %v", bluesky.ServerBskySocial, err)
	}

	return bc
}

func initMastodonClient(db *bolt.DB, instanceName string, appId, appSecret *string) *madon.Client {
	var client *madon.Client

	if appId != nil && appSecret != nil {
		log.Printf("Mastodon: restoring client from environment variables")
		mc, err := madon.RestoreApp(
			AppName,
			instanceName,
			*appId,
			*appSecret,
			nil)
		if err != nil {
			log.Fatalf("could not restore client: %v", err)
		}
		client = mc
	} else {
		if appId != nil && appSecret == nil {
			log.Printf("WARNING: VBC_MASTODON_APP_SECRET is not set when VCB_MASTODON_APP_ID is, ignoring.")
		} else if appSecret != nil && appId == nil {
			log.Printf("WARNING: VBC_MASTODON_APP_ID is not set when VCB_MASTODON_APP_SECRET is, ignoring.")
		}

		/* If we're already registered, don't register again. */
		err := db.View(func(tx *bolt.Tx) error {
			bucket := tx.Bucket([]byte(instanceName))
			if bucket == nil {
				return nil
			}

			var appId []byte
			var appSecret []byte

			storedAppId := bucket.Get([]byte(AppIdKey))
			storedAppSecret := bucket.Get([]byte(AppSecretKey))

			if storedAppId != nil {
				appId = copySlice[byte](storedAppId)
			}
			if storedAppSecret != nil {
				appSecret = copySlice[byte](storedAppSecret)
			}

			if appId == nil || storedAppId == nil {
				return nil
			}

			log.Printf("Mastodon: restoring client from store")
			mc, err := madon.RestoreApp(
				AppName,
				instanceName,
				string(appId),
				string(appSecret),
				nil)
			if err != nil {
				return err
			}

			client = mc
			return nil
		})
		if err != nil {
			log.Fatalf("could not restore client info from store: %v", err)
		}
	}

	/* We're gonna have to register our app. */
	if client == nil {
		log.Printf("Mastodon: creating client from new app")
		mc, err := madon.NewApp(
			AppName,
			AppWebsite,
			[]string{"read:statuses"},
			madon.NoRedirect,
			instanceName)
		if err != nil {
			log.Fatalf("could not register new app: %v", err)
		}

		/* Save it to the store. */
		err = db.Update(func(tx *bolt.Tx) error {
			bucket, err := tx.CreateBucketIfNotExists([]byte(instanceName))
			if err != nil {
				return err
			}

			err = bucket.Put([]byte(AppIdKey), []byte(mc.ID))
			if err != nil {
				return err
			}

			err = bucket.Put([]byte(AppSecretKey), []byte(mc.Secret))
			if err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			log.Printf("WARNING: App ID and secret could not be saved to the s-")
			log.Printf("WARNING: tore. Please, use the following environment va-")
			log.Printf("WARNING: riables going forward:")
			log.Printf("WARNING:")
			log.Printf("WARNING: VBC_MASTODON_APP_ID=\"%v\"", mc.ID)
			log.Printf("WARNING: VBC_MASTODON_APP_SECRET=\"%v\"", mc.Secret)
		}
		client = mc
	}

	return client
}

func requireEnv(name string) string {
	value, found := os.LookupEnv(name)
	if !found {
		log.Fatalf("could not find required env %v", name)
	}
	return value
}

func envOrDefault(name string, def string) string {
	value, found := os.LookupEnv(name)
	if !found {
		return def
	} else {
		return value
	}
}

func envOrNil(name string) *string {
	value, found := os.LookupEnv(name)
	if !found {
		return nil
	} else {
		return &value
	}
}

func copySlice[T any](src []T) []T {
	dst := make([]T, len(src))
	copy(dst, src)

	return dst
}
