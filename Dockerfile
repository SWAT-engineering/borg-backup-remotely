FROM alpine:3.19
RUN apk add --no-cache openssh borgbackup
ENTRYPOINT ["/usr/bin/borg-backup-remotely"]
COPY borg-backup-remotely /usr/bin/borg-backup-remotely