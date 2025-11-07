FROM ubuntu:22.04
RUN apt-get update && apt-get install -y wget
RUN wget https://developer.download.nvidia.com/compute/nsight-systems/2025.5.1/nsight-systems-2025.5.1-cli-linux-x86_64.deb && \
    apt-get install -y ./nsight-systems-2025.5.1-cli-linux-x86_64.deb && \
    rm nsight-systems-2025.5.1-cli-linux-x86_64.deb
CMD ["bash"]