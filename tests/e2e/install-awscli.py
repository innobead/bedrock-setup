"""Installs AWS CLI v2 in the image without curl or unzip (used by the Dockerfile)."""
import os
import platform
import subprocess
import urllib.request
import zipfile

url = f"https://awscli.amazonaws.com/awscli-exe-linux-{platform.machine()}.zip"
urllib.request.urlretrieve(url, "/tmp/awscli.zip")
with zipfile.ZipFile("/tmp/awscli.zip") as z:
    for info in z.infolist():
        path = z.extract(info, "/tmp")
        mode = info.external_attr >> 16
        if mode:
            os.chmod(path, mode)
subprocess.run(["/tmp/aws/install"], check=True)
