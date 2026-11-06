FROM python:3.11-slim-trixie
ARG REVISION
LABEL org.opencontainers.image.revision=$REVISION
COPY server.py /app/sub2api
COPY entrypoint.sh /app/docker-entrypoint.sh
RUN chmod 755 /app/sub2api /app/docker-entrypoint.sh
ENTRYPOINT ["/app/docker-entrypoint.sh"]
CMD ["/app/sub2api"]
HEALTHCHECK --interval=2s --timeout=1s --start-period=1s --retries=2 CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/health', timeout=1).read()"
