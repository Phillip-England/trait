# The Environment File

#env #config #docker

This application makes use of an env file. You should be able to run some sort of command using this app to initialize a `.env` file.

When running this application, pass the path to the env file as an argument. On the server, initialize a `.env` file, place it in `/etc`, and tell Docker about the location of the env file when launching.

That way all configuration for this app lives in a single environment file and Dockerizing the application later stays simple.
