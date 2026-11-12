#!/usr/bin/python3
"""Authenticate the enrolled worker updater; never run a board or provider."""

import base64
import datetime
import http.client
import json
import os
from pathlib import Path
import ssl
import stat
import subprocess
import sys
import time

CONFIG = "/etc/sybra/worker-github-app.json"
KEY = "/etc/sybra/worker-github-app.pem"
UPDATER = "/usr/local/lib/sybra/bin/sybra-worker-update"
UPDATE_CONFIG = "/etc/sybra/worker-update.yaml"
SAFE_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
OPENSSL = "/usr/bin/openssl"
REPOSITORY = "Automaat/sybra"


class AuthError(Exception):
    """Only fixed, non-secret diagnostics may escape the authentication path."""


def trusted_path(name, private=False):
    path = Path(name)
    if not path.is_absolute() or str(path) != name or ".." in path.parts:
        raise AuthError("authentication paths must be absolute and canonical")
    for current in (path, *path.parents):
        info = current.lstat()
        if stat.S_ISLNK(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
            raise AuthError("authentication path is not root-controlled")
        if current == path:
            if not stat.S_ISREG(info.st_mode) or (private and info.st_mode & 0o077):
                raise AuthError("authentication file has unsafe type or permissions")
        elif not stat.S_ISDIR(info.st_mode):
            raise AuthError("authentication parent is not a directory")


def load_config():
    trusted_path(CONFIG, private=True)
    trusted_path(KEY, private=True)
    with open(CONFIG, "rb") as stream:
        data = stream.read(4097)
    if len(data) > 4096:
        raise AuthError("authentication configuration is too large")
    config = json.loads(data)
    if not isinstance(config, dict) or set(config) != {"app_id", "installation_id"}:
        raise AuthError("invalid GitHub App configuration fields")
    if any(type(value) is not int or value <= 0 for value in config.values()):
        raise AuthError("GitHub App identifiers must be positive integers")
    return config


def encode(data):
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def make_jwt(app_id):
    now = int(time.time())
    header = encode(b'{"alg":"RS256","typ":"JWT"}')
    payload = encode(json.dumps({"iat": now - 60, "exp": now + 540, "iss": str(app_id)}).encode())
    message = f"{header}.{payload}"
    signed = subprocess.run(
        [OPENSSL, "dgst", "-sha256", "-sign", KEY],
        input=message.encode("ascii"), capture_output=True, timeout=15,
        check=False, env={"PATH": SAFE_PATH},
    )
    if signed.returncode != 0 or not signed.stdout:
        raise AuthError("GitHub App signing failed")
    return f"{message}.{encode(signed.stdout)}"


def api(method, path, token, body=None):
    # Fixed public origin, system CA bundle, no proxy or redirect processing.
    context = ssl.create_default_context(cafile="/etc/ssl/certs/ca-certificates.crt")
    connection = http.client.HTTPSConnection("api.github.com", timeout=20, context=context)
    try:
        connection.request(method, path, body=json.dumps(body) if body is not None else None,
                           headers={"Authorization": "Bearer " + token,
                                    "Accept": "application/vnd.github+json",
                                    "Content-Type": "application/json",
                                    "User-Agent": "sybra-worker-update",
                                    "X-GitHub-Api-Version": "2026-03-10"})
        response = connection.getresponse()
        if response.status != (201 if method == "POST" else 200):
            # Never include GitHub's response body: it may echo credentials.
            raise AuthError(f"GitHub authentication request returned HTTP {response.status}")
        data = response.read((1 << 20) + 1)
        if len(data) > 1 << 20:
            raise AuthError("GitHub authentication response exceeded limit")
        return json.loads(data)
    finally:
        connection.close()


def installation_token(config):
    result = api("POST", f"/app/installations/{config['installation_id']}/access_tokens",
                 make_jwt(config["app_id"]),
                 {"repositories": ["sybra"], "permissions": {"actions": "read"}})
    token = result.get("token") if isinstance(result, dict) else None
    if not isinstance(token, str) or not token or len(token) > 16384 or not token.isascii() or any(c.isspace() for c in token):
        raise AuthError("GitHub returned an invalid installation token")
    permissions = result.get("permissions", {})
    if not isinstance(permissions, dict) or permissions.get("actions") != "read" or any(
        key not in {"actions", "metadata"} or value != "read" for key, value in permissions.items()
    ):
        raise AuthError("GitHub returned unexpected installation token permissions")
    expires = datetime.datetime.fromisoformat(result["expires_at"].replace("Z", "+00:00"))
    remaining = expires.timestamp() - time.time()
    if expires.tzinfo is None or not 900 < remaining <= 4500:
        raise AuthError("GitHub installation token lifetime is unsuitable")
    repos = api("GET", "/installation/repositories?per_page=2", token)
    if not isinstance(repos, dict) or repos.get("total_count") != 1 or [
        item.get("full_name") for item in repos.get("repositories", [])
    ] != [REPOSITORY]:
        raise AuthError("GitHub installation token is not limited to the expected repository")
    return token


def updater_environment(token):
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(("GH_", "GITHUB_")) and key.lower() not in
           {"http_proxy", "https_proxy", "all_proxy", "no_proxy"}}
    env.update(GH_HOST="github.com", GH_PROMPT_DISABLED="1",
               GH_CONFIG_DIR="/var/lib/sybra-worker-update/gh", PATH=SAFE_PATH,
               XDG_CACHE_HOME="/var/lib/sybra-worker-update/cache")
    if token is not None:
        env["GH_TOKEN"] = token
    return env


def main():
    try:
        if os.geteuid() != 0 or sys.argv[1:] not in ([], ["--check-auth"]):
            raise AuthError("run as root, optionally with --check-auth")
        trusted_path(UPDATER)
        trusted_path(UPDATE_CONFIG)
        try:
            token = installation_token(load_config())
        except Exception:
            if sys.argv[1:]:
                raise
            # Authentication is needed to stage a release, never to recover an
            # already-held activation. Keep the helper's state machine running
            # without ambient credentials when GitHub or signing is unavailable.
            print("worker updater authentication unavailable; continuing without GitHub credentials for recovery",
                  file=sys.stderr)
            token = None
        if sys.argv[1:]:
            print("GitHub App authentication verified: Automaat/sybra, Actions read")
            return 0
        # No token file, command-line token, or persistent gh auth login.
        os.execve(UPDATER, [UPDATER, "-config", UPDATE_CONFIG], updater_environment(token))
    except AuthError as error:
        print(f"worker updater authentication: {error}", file=sys.stderr)
    except Exception:
        # Parser/OS/transport exceptions can contain input data. Do not log them.
        print("worker updater authentication: configuration, signing, or transport failed", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
