run:
	go run main.go serve -env .env

docker:
	docker build -t receipt-upload . && docker run --rm \
                -p 8725:8725 \
                -v $(CURDIR)/config:/app/config \
                -v $(CURDIR)/data:/app/data \
                receipt-upload
