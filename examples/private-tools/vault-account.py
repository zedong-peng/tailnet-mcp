#!/usr/bin/env python3
"""Invite an email to Vaultwarden, and optionally register it with a master password.

Signups are closed, so every account starts as an admin invitation. bw has no register command,
so for an agent's account this script does what the web vault does on sign-up: derive the master key,
wrap a fresh user key and RSA key pair with it, and post the result. Run on the server:

  VW_ADMIN_TOKEN=... vault-account.py invite you@example.com
  VW_ADMIN_TOKEN=... VAULT_PASSWORD=... vault-account.py register my-agent@vault.example
"""

import base64
import hashlib
import hmac
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

from cryptography.hazmat.primitives import hashes, padding, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes
from cryptography.hazmat.primitives.kdf.hkdf import HKDFExpand

VW = os.environ.get("VW_URL", "http://127.0.0.1:8222")
ITERATIONS = 600_000
b64 = lambda b: base64.b64encode(b).decode()

cookie = ""  # the admin session; kept by hand because Vaultwarden may mark it Secure on plain http


def post(path, body, form=False, accept_json=True):
    global cookie
    data = urllib.parse.urlencode(body).encode() if form else json.dumps(body).encode()
    headers = {"Content-Type": "application/x-www-form-urlencoded" if form else "application/json"}
    if accept_json:
        headers["Accept"] = "application/json"
    if cookie:
        headers["Cookie"] = cookie
    req = urllib.request.Request(VW + path, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req) as r:
            if set_cookie := r.headers.get("Set-Cookie"):
                cookie = set_cookie.split(";", 1)[0]
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def invite(email):
    status, _ = post("/admin", {"token": os.environ["VW_ADMIN_TOKEN"]}, form=True, accept_json=False)
    if status != 200 or not cookie:
        sys.exit(f"admin login failed ({status})")
    status, body = post("/admin/invite", {"email": email})
    if status == 200:
        print(f"invited {email}")
    elif "already exists" in body:
        print(f"{email} is already invited or registered")
    else:
        sys.exit(f"invite failed ({status}): {body}")


def enc_string(plain, enc_key, mac_key):
    """Bitwarden EncString type 2: AES-256-CBC + HMAC-SHA256."""
    iv = os.urandom(16)
    padder = padding.PKCS7(128).padder()
    enc = Cipher(algorithms.AES(enc_key), modes.CBC(iv)).encryptor()
    ct = enc.update(padder.update(plain) + padder.finalize()) + enc.finalize()
    mac = hmac.new(mac_key, iv + ct, hashlib.sha256).digest()
    return f"2.{b64(iv)}|{b64(ct)}|{b64(mac)}"


def register(email, password):
    email = email.strip().lower()
    master_key = hashlib.pbkdf2_hmac("sha256", password.encode(), email.encode(), ITERATIONS, 32)
    master_hash = hashlib.pbkdf2_hmac("sha256", master_key, password.encode(), 1, 32)
    stretch = lambda info: HKDFExpand(hashes.SHA256(), 32, info).derive(master_key)
    user_key = os.urandom(64)  # 32-byte encryption key + 32-byte MAC key

    private = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    public_der = private.public_key().public_bytes(
        serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)
    private_der = private.private_bytes(
        serialization.Encoding.DER, serialization.PrivateFormat.PKCS8, serialization.NoEncryption())

    status, token = post("/identity/accounts/register/send-verification-email",
                         {"email": email, "name": email.split("@")[0], "receiveMarketingEmails": False},
                         accept_json=False)
    if status != 200:
        sys.exit(f"registration not allowed ({status}): {token}")
    status, body = post("/identity/accounts/register/finish", {
        "email": email,
        "emailVerificationToken": token.strip().strip('"'),
        "masterPasswordHash": b64(master_hash),
        "masterPasswordHint": None,
        "userSymmetricKey": enc_string(user_key, stretch(b"enc"), stretch(b"mac")),
        "userAsymmetricKeys": {
            "publicKey": b64(public_der),
            "encryptedPrivateKey": enc_string(private_der, user_key[:32], user_key[32:]),
        },
        "kdf": 0,
        "kdfIterations": ITERATIONS,
    })
    if status != 200:
        sys.exit(f"register failed ({status}): {body}")
    print(f"registered {email}")


if __name__ == "__main__":
    if len(sys.argv) != 3 or sys.argv[1] not in ("invite", "register"):
        sys.exit(__doc__)
    cmd, email = sys.argv[1:]
    invite(email)
    if cmd == "register":
        register(email, os.environ["VAULT_PASSWORD"])
