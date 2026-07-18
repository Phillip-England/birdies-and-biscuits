run:
	go run . serve

init:
	go run . init

docker:
	docker build -t birdies-and-biscuits . && docker run --rm \
                -p 8777:8777 \
                -v $(CURDIR)/config:/app/config \
                -v $(CURDIR)/data:/app/data \
                birdies-and-biscuits
