.PHONY: docker

docker:
	mkdir -p config data public/uploads
	docker build -t artif4ct . && docker run --rm \
		-p 6688:6688 \
		-v $(CURDIR)/config:/app/config \
		-v $(CURDIR)/data:/app/data \
		-v $(CURDIR)/public/uploads:/app/public/uploads \
		artif4ct
