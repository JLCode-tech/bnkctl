FROM public.ecr.aws/docker/library/python:3.14-slim
WORKDIR /var/task
COPY . .
RUN apt-get update && apt-get install -y curl dnsutils && rm -rf /var/lib/apt/lists/*
RUN pip install uv && uv pip install --system -r pyproject.toml
CMD [ "python", "main.py" ]
