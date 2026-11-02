# tw-sync

Runs Taskwarrior 3 sync to S3 with credentials kept in the macOS Keychain.

Taskwarrior can't use `credential_process` in an AWS profile, so `tw-sync` reads
its secrets from the Keychain and passes them to a single `task sync` process
through the environment. Nothing secret is written to a taskrc file.

## Install

```sh
go install github.com/rscottlundgren/tw-sync@latest
```

## AWS

`infra/template.yaml` creates a private, versioned bucket, a policy limited to
that bucket, and one IAM user per device. Deploy it with an admin profile, then
create an access key for each device user by hand so no secret enters the stack.

```sh
aws cloudformation deploy --template-file infra/template.yaml \
  --stack-name per-taskwarrior-sync --capabilities CAPABILITY_NAMED_IAM
```

## Use

```sh
tw-sync setup --device per|harvard --bucket <bucket>
tw-sync now
tw-sync agent install
tw-sync doctor
```

`setup` backs up existing tasks, then asks for the access key ID, the secret
access key and the encryption secret. The secret must match on every device. A
second device must start empty, and `setup` stops if it doesn't.

`agent install` adds a launchd job that syncs every 15 minutes. Logs are in
`~/Library/Logs/tw-sync/`. Changes that have synced can no longer be undone with
`task undo`.
