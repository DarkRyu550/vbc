# vbc
Matt's Very Bad™ Crossposter from Mastodon to Bluesky

Watches your posts on Mastodon and reposts them to Bluesky. And is also very 
bad™. 

## Usage Guide
If for some cursed reason you figure you want to use this, here's how you
can get set up. First, set the following enviroment variables:

- `VBC_BSKY_HANDLE`: Your Bluesky handle (no leading `@`!)
- `VBC_BSKY_APP_KEY`: The app key you wish to use for the crossposter.
- `VBC_MASTODON_ACCOUNT_ID`: The ID of your Mastodon account. This is a number,
different from your handle. If you don't know what your account ID is and want
to figure it out, just use 
`curl https://<your-instance>/api/v1/accounts/lookup?acct=<your-handle> | jq '.id'`.
- `VBC_MASTODON_INSTANCE`: The URL of your instance. Don't forget to start this
value with `https://`!

Optionally, you may also want to set:
- `VBC_STORE_FILE`: Controls which file will be used for the persistant store.
Not setting this value will make `vbc` default to `vbc.bolt` as the file name.

When you're done with that, simply run:
```sh
go run vbc/main.go
```

