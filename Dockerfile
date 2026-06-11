FROM golang:1.26-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/agenthub-sandbox ./cmd/sandbox

FROM debian:bookworm-slim

RUN apt-get update \
	&& apt-get install -y --no-install-recommends git ca-certificates bash tini nodejs npm \
	&& npm install -g vercel \
	&& rm -rf /var/lib/apt/lists/* ~/.npm

RUN git config --system --add safe.directory '*'

RUN useradd --create-home --uid 10001 --shell /bin/bash sandbox

WORKDIR /home/sandbox

COPY --from=build /out/agenthub-sandbox /usr/local/bin/agenthub-sandbox

ENV HOST=0.0.0.0
ENV PORT=8080
ENV REPO_ROOT=/sandbox/views/workspace/repo
ENV WORKTREE_ROOT=/sandbox/views/workspace/worktrees

USER sandbox

EXPOSE 8080

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["agenthub-sandbox"]
