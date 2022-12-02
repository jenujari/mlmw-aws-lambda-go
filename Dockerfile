FROM public.ecr.aws/lambda/provided:al2 as build
# install compiler
RUN yum install -y golang
RUN go env -w GOPROXY=direct
# cache dependencies
ADD go.mod ./
ADD go.sum ./
ADD main.go ./
RUN go mod download
RUN go build -o /main
# copy artifacts to a clean image
FROM public.ecr.aws/lambda/provided:al2
RUN yum install -y wget unzip
WORKDIR "/home"
RUN wget -O chromium.zip https://github.com/adieuadieu/serverless-chrome/releases/download/v1.0.0-57/stable-headless-chromium-amazonlinux-2.zip
RUN unzip ./chromium.zip
RUN rm ./chromium.zip
RUN mv -f ./headless-chromium /usr/local/bin/google-chrome
RUN chmod 755 /usr/local/bin/google-chrome
COPY --from=build /main /home/app
# ADD https://github.com/aws/aws-lambda-runtime-interface-emulator/releases/latest/download/aws-lambda-rie /usr/bin/aws-lambda-rie
# RUN chmod 755 /usr/bin/aws-lambda-rie
# ENTRYPOINT [ "/bin/bash", "-c", "--","tail -f /dev/null" ]
# COPY entry.sh /
# RUN chmod 755 /entry.sh
# ENTRYPOINT [ "/entry.sh" ]
ENTRYPOINT [ "/home/app" ]           