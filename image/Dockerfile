FROM ubuntu:22.04

RUN apt-get update && \
    apt-get install -y git curl wget iproute2 iputils-ping jq strace build-essential python3

RUN \
    curl -o /usr/bin/rustup https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-gnu/rustup-init && \
    chmod +x /usr/bin/rustup && \
    ln -s /usr/bin/rustup /usr/bin/cargo && \
    ln -s /usr/bin/rustup /usr/bin/cargo-clippy && \
    ln -s /usr/bin/rustup /usr/bin/cargo-fmt && \
    ln -s /usr/bin/rustup /usr/bin/cargo-miri && \
    ln -s /usr/bin/rustup /usr/bin/clippy-driver && \
    ln -s /usr/bin/rustup /usr/bin/rust-gdb && \
    ln -s /usr/bin/rustup /usr/bin/rust-lldb && \
    ln -s /usr/bin/rustup /usr/bin/rustc && \
    ln -s /usr/bin/rustup /usr/bin/rustdoc && \
    ln -s /usr/bin/rustup /usr/bin/rustfmt

RUN \
    curl -L -o /usr/bin/cargo-batch https://github.com/embassy-rs/cargo-batch/releases/download/batch-0.3.0/cargo-batch && \
    chmod +x /usr/bin/cargo-batch
