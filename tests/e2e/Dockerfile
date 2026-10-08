# Everything the e2e tests need at runtime. Built and run by run.sh.
# Downloads only from PyPI and awscli.amazonaws.com (no apt), and works with or without BuildKit.
# The CLIs under test are not in the image: run.sh builds them into tests/e2e/bin and mounts the repo.
FROM python:3.13-slim-bookworm

# AWS CLI v2 (for aws sso login inside the container and for debugging in ./run.sh shell),
# unpacked with Python so the image needs no curl or unzip.
COPY install-awscli.py /tmp/
RUN python /tmp/install-awscli.py && rm -rf /tmp/aws /tmp/awscli.zip /tmp/install-awscli.py

COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt \
    && aws --version && python -c "import robot, boto3; print('robot', robot.__version__, 'boto3', boto3.__version__)"

# run.sh runs the container as the host user, so HOME must be writable by any uid.
# ~/.aws is bind-mounted under it.
RUN mkdir -p -m 1777 /home/e2e
ENV HOME=/home/e2e \
    E2E_IN_CONTAINER=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    AWS_PAGER=""
WORKDIR /work/tests/e2e
