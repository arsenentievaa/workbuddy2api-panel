# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit

FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
# py3-pip + PyMuPDF (venv /opt/pdf)：把 PDF 渲染成图片。上游不接受任何文档块
# （四种形态实测均被拒，见 internal/upstream/pdfdoc.go），文档分析只剩图像这条路，
# 渲染交给 scripts/pdf_to_images.py。
# 安装是「尽力而为」：构建时一次网络或套件故障不应让整个镜像失败。失败时渲染脚本
# 会报告 pymupdf 缺失，PDF 块按原样透传给上游——与加入本功能之前的行为一致，
# 即「退回到已知状态」而不是「构建不出来」。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash py3-pip \
 && ( python3 -m venv /opt/pdf \
      && /opt/pdf/bin/pip install --no-cache-dir --quiet pymupdf \
      && /opt/pdf/bin/python -c "import pymupdf; print('PyMuPDF', pymupdf.__version__)" \
      || echo "AVERTISSEMENT: PyMuPDF indisponible — les PDF seront transmis tels quels" ) \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY login.sh signin.sh credit.sh /app/
COPY scripts/probe_active.py /app/scripts/probe_active.py
# 渲染脚本用 venv 的解释器（ENV 里的 PDF_PYTHON）执行；PDF_PYTHON=/usr/bin/python3
# 可回退到系统解释器（无 pymupdf 时脚本会退出 3，PDF 块原样透传）。
COPY scripts/pdf_to_images.py /app/scripts/pdf_to_images.py
ENV PDF_PYTHON=/opt/pdf/bin/python
RUN sed -i 's/\r$//' /app/login.sh /app/signin.sh /app/credit.sh && chmod 755 /app/login.sh /app/signin.sh /app/credit.sh
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]