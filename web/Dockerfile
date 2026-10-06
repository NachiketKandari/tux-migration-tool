# ---- Go: build the tuxconv CLI the web server shells out to ----
FROM golang:1.23-alpine AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /tuxconv ./cmd/tuxconv

# ---- Node: build the Next.js standalone server ----
FROM node:20-alpine AS nodebuild
WORKDIR /web
# No phone-home at build or runtime.
ENV NEXT_TELEMETRY_DISABLED=1
COPY web/package.json web/package-lock.json* ./
RUN npm ci --no-audit --no-fund && npx next telemetry disable
COPY web/ ./
RUN npm run build

# ---- Runtime: single process, no dev deps ----
FROM node:20-alpine
WORKDIR /app
# TUXCONV_CONFIG is unset here by default (stock defaults + container env).
# To share yaml keys with browser runs, mount a gitignored yaml and set it:
#   docker run -p 127.0.0.1:3000:3000 -v $PWD/.tuxconv.yaml:/cfg.yaml:ro -e TUXCONV_CONFIG=/cfg.yaml tux-web
ENV NODE_ENV=production TUXCONV_BIN=/app/bin/tuxconv NEXT_TELEMETRY_DISABLED=1
COPY --from=gobuild /tuxconv /app/bin/tuxconv
COPY --from=nodebuild /web/.next/standalone ./
COPY --from=nodebuild /web/.next/static ./.next/static
COPY --from=nodebuild /web/public ./public
EXPOSE 3000
CMD ["node", "server.js"]
