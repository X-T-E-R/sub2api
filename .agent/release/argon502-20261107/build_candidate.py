#!/usr/bin/env python3
"""Build only the frozen 7c12 source, save/load it and inspect a never-started container."""
import datetime as dt
import gzip
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys

SOURCE = '7c12de7380be0142e18bcdc6397b6537f2af9968'
PARENT = 'b0e0fb9e671c0180d8e39c9ac5dffebdbc943bd9'
HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
ARTIFACTS = REPO / '.tmp' / 'argon502-release-20261107'
TAG = 'sub2api-argon502:7c12de738'
DOCKER = ['docker', '--context', 'desktop-linux']
BASE_IMAGES = {
    'NODE_IMAGE': 'node:24-alpine@sha256:8a32c8cf649a49915a66e81e7a609223c708d0adabf288ee3f4077d765542f85',
    'ALPINE_IMAGE': 'alpine:3.21@sha256:ee16d92f96db08d9d2d07da48555a27f3a69d8f0759021dbc601195ed0d2b5bd',
    'POSTGRES_IMAGE': 'postgres:18-alpine@sha256:eaef288f54a90a909e49f0121d5d047a5e974e922699de4249625cf7cde34f36',
    'GOLANG_IMAGE': 'golang:1.27.3-alpine@sha256:fa4881266890ef051f0ce63afdfaccb69bbe8186084387f4fafd60d7ecbabc71',
}


def output(command, timeout=30):
    return subprocess.run(command, cwd=REPO, check=True, capture_output=True, text=True, timeout=timeout).stdout.strip()


def main():
    ARTIFACTS.mkdir(parents=True, exist_ok=True)
    engine = json.loads(output(DOCKER + ['info', '--format', '{{json .}}'], timeout=15))
    if engine['OSType'] != 'linux' or engine['Architecture'] not in ('x86_64', 'amd64'):
        raise RuntimeError('local_engine_platform_changed')
    parent = output(['git', 'rev-parse', SOURCE + '^'])
    if parent != PARENT:
        raise RuntimeError('frozen_source_parent_changed')
    version = output(['git', 'show', SOURCE + ':backend/cmd/server/VERSION'])
    built_at = dt.datetime.now(dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    source_tar = ARTIFACTS / 'source-7c12de738.tar'
    output(['git', 'archive', '--format=tar', '--output=' + str(source_tar), SOURCE])
    build = DOCKER + ['buildx', 'build', '--progress=plain', '--platform', 'linux/amd64',
                      '--provenance=false', '--sbom=false', '--build-arg', 'VERSION=' + version,
                      '--build-arg', 'COMMIT=' + SOURCE, '--build-arg', 'DATE=' + built_at]
    for key, value in BASE_IMAGES.items():
        build += ['--build-arg', key + '=' + value]
    build += ['--label', 'org.opencontainers.image.revision=' + SOURCE, '--tag', TAG, '--load', '-']
    with source_tar.open('rb') as context:
        subprocess.run(build, cwd=REPO, stdin=context, check=True, timeout=1800)
    before = json.loads(output(DOCKER + ['image', 'inspect', TAG]))[0]
    image_tar = ARTIFACTS / 'sub2api-argon502-7c12de738-linux-amd64.tar'
    archive = image_tar.with_suffix('.tar.gz')
    output(DOCKER + ['save', '--platform', 'linux/amd64', '--output', str(image_tar), TAG], timeout=180)
    with image_tar.open('rb') as source, archive.open('wb') as target:
        with gzip.GzipFile(filename='', mode='wb', compresslevel=6, fileobj=target, mtime=0) as compressed:
            shutil.copyfileobj(source, compressed)
    output(DOCKER + ['load', '--platform', 'linux/amd64', '--input', str(archive)], timeout=180)
    loaded = json.loads(output(DOCKER + ['image', 'inspect', TAG]))[0]
    if loaded['Id'] != before['Id'] or loaded['Os'] != 'linux' or loaded['Architecture'] != 'amd64':
        raise RuntimeError('loaded_image_identity_or_platform_changed')
    if loaded['Config']['Labels']['org.opencontainers.image.revision'] != SOURCE:
        raise RuntimeError('loaded_image_source_changed')
    container = output(DOCKER + ['create', '--network', 'none', '--entrypoint', '/app/sub2api',
                                 '--name', 'sub2-argon502-proof-' + built_at.replace(':', '').replace('-', ''), TAG, '-version'])
    try:
        probe = json.loads(output(DOCKER + ['inspect', container]))[0]
        if probe['Image'] != loaded['Id'] or probe['State']['Running'] or probe['State']['Status'] != 'created':
            raise RuntimeError('never_started_probe_identity_changed')
        if probe['Config']['Labels']['org.opencontainers.image.revision'] != SOURCE:
            raise RuntimeError('probe_revision_changed')
    finally:
        output(DOCKER + ['rm', container])
    with archive.open('rb') as data:
        transfer_sha256 = hashlib.file_digest(data, 'sha256').hexdigest()
    candidate = {
        'source_revision': SOURCE, 'parent_revision': PARENT, 'image_runtime_id': loaded['Id'], 'tag': TAG,
        'platform': 'linux/amd64', 'archive': archive.as_posix(), 'bytes': archive.stat().st_size,
        'transfer_sha256': transfer_sha256, 'version': version, 'built_at': built_at,
        'build_context': 'git archive ' + SOURCE, 'base_images': BASE_IMAGES,
        'save_load_create': {'result': 'passed', 'container_id': container, 'status': probe['State']['Status'],
                             'running': probe['State']['Running'], 'image_runtime_id': probe['Image'],
                             'oci_revision': probe['Config']['Labels']['org.opencontainers.image.revision'],
                             'probe_removed': True},
        'engine': {'context': 'desktop-linux', 'version': engine['ServerVersion'], 'os': engine['OSType'], 'architecture': engine['Architecture']},
    }
    (ARTIFACTS / 'candidate.json').write_text(json.dumps(candidate, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(candidate, indent=2))


if __name__ == '__main__':
    try:
        main()
    except subprocess.TimeoutExpired as error:
        print(json.dumps({'state': 'blocked', 'command': error.cmd, 'timeout_seconds': error.timeout}), file=sys.stderr)
        raise SystemExit(2)
