FROM golang:1.26
WORKDIR /app
COPY . .
RUN go build -o /usr/local/bin/birdies-and-biscuits .
EXPOSE 8777
CMD ["birdies-and-biscuits"]
