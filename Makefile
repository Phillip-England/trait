.PHONY: docker

docker:
	mkdir -p config data public/uploads
	docker build -t trait . && docker run --rm \
		-p 6688:6688 \
		-v $(CURDIR)/config:/app/config \
		-v $(CURDIR)/data:/app/data \
		trait
