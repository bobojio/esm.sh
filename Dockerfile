FROM golang:1.16

EXPOSE 80

WORKDIR /

RUN apt-get update -y && apt-get install -y xz-utils

# ADD . /esm.sh
# v43
RUN git clone https://github.com/postui/esm.sh \
    && cd esm.sh \
    && git checkout b11caf3

WORKDIR /esm.sh

RUN sh ./scripts/build.sh

CMD ["./scripts/esmd", "-dev", "-domain", "localhost"]
