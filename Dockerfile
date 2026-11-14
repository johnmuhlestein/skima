# official Docker Hub images via AWS's public mirror, which avoids Docker Hub's anonymous pull rate limit
FROM public.ecr.aws/docker/library/golang:1.25-alpine AS build

WORKDIR /app

# Download modules first so this layer is cached until go.mod/go.sum change
COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY cmd cmd
COPY internal internal
COPY schemas schemas

# the image has no git metadata, so the version is passed in: docker build --build-arg VERSION=v1.2.3
ARG VERSION=""
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/johnmuhlestein/skima/cmd.version=${VERSION}" -o /out/skima .

FROM public.ecr.aws/docker/library/alpine:3.22

# NOTE: curl is needed for the entrypoint script check for the istio sidecar
RUN apk add --no-cache curl \
    && addgroup -S app \
    && adduser -S -G app -H -h /skima app

WORKDIR /skima

COPY --chown=app:app --from=build /out/skima /skima/skima
COPY --chown=app:app --chmod=755 entrypoint.sh /skima/entrypoint.sh

USER app

ENTRYPOINT ["/skima/entrypoint.sh", "/skima/skima"]
CMD ["-h"]
